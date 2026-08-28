package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
	providerprofilesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/providerprofile"
	workflowsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/workflow"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// WorkflowOwnershipStore backs Checkpoint 8P-A's minimal workflow-run
// ownership scoping — see OwnershipStore's own doc comment in projects.go
// for the same reasoning applied to workflow runs.
type WorkflowOwnershipStore interface {
	GetWorkflowRunOwner(ctx context.Context, id string) (*domain.UserID, error)
	SetWorkflowRunOwner(ctx context.Context, id string, owner domain.UserID) (bool, error)
}

// WorkflowProjectIDParam is the {projectId} path parameter for the
// project-scoped workflow-run creation route.
type WorkflowProjectIDParam struct {
	ProjectID string `path:"projectId" description:"Project identifier (registry key)."`
}

// WorkflowIDParam is the {workflowId} path parameter for workflow-run routes.
type WorkflowIDParam struct {
	WorkflowID string `path:"workflowId" description:"Workflow run identifier."`
}

// WorkflowTaskParam addresses one planned task of a master run.
type WorkflowTaskParam struct {
	WorkflowID string `path:"workflowId" description:"Master workflow run identifier."`
	TaskID     string `path:"taskId" description:"Planned task identifier (workflow_tasks.id)."`
}

// ListWorkflowsQuery is the query string accepted by GET /api/v1/workflows.
type ListWorkflowsQuery struct {
	ProjectID string `query:"projectId,omitempty" description:"Project id filter."`
}

// CreateWorkflowRunRequest is the body of POST /api/v1/projects/{projectId}/workflows.
type CreateWorkflowRunRequest struct {
	Objective        string                          `json:"objective" description:"The workflow run's objective."`
	Verification     WorkflowVerificationPlan        `json:"verification,omitempty"`
	MasterPlan       bool                            `json:"masterPlan,omitempty" description:"Generate a provider-neutral master plan before execution."`
	PlanApprovalMode domain.WorkflowPlanApprovalMode `json:"planApprovalMode,omitempty" enum:"manual,auto"`
	// Autonomous is Checkpoint 8P-D.1's explicit per-run Manual/Autonomous
	// choice, made in the create-workflow UI rather than only inferred from
	// the caller's global UserExecutionPolicy.AutonomousMode setting. Nil
	// (field omitted) preserves the pre-8P-D.1 behavior of inheriting
	// whatever the caller's stored/default execution policy says. Non-nil
	// overrides AutonomousMode in this run's own frozen policy snapshot only
	// -- it never writes back to the caller's stored UserExecutionPolicy.
	Autonomous *bool `json:"autonomous,omitempty" description:"Explicit per-run autonomous/manual override; omit to inherit the caller's execution policy."`
}

type WorkflowVerificationPlan struct {
	Commands []WorkflowVerificationCommand `json:"commands,omitempty"`
	Files    []WorkflowVerificationFile    `json:"files,omitempty"`
}
type WorkflowVerificationCommand struct {
	Command          string   `json:"command"`
	Args             []string `json:"args,omitempty"`
	WorkingDirectory string   `json:"workingDirectory,omitempty"`
	TimeoutSeconds   int      `json:"timeoutSeconds,omitempty"`
	RequiredExitCode int      `json:"requiredExitCode"`
	RetrySafe        bool     `json:"retrySafe"`
}
type WorkflowVerificationFile struct {
	Path         string  `json:"path"`
	Exists       bool    `json:"exists"`
	ExactContent *string `json:"exactContent,omitempty"`
	SHA256       string  `json:"sha256,omitempty"`
}

// WorkflowAttemptView is one execution attempt of a workflow step.
type WorkflowAttemptView struct {
	ID            string                        `json:"id"`
	AttemptNumber int64                         `json:"attemptNumber"`
	Harness       string                        `json:"harness,omitempty"`
	Model         string                        `json:"model,omitempty"`
	StartedAt     time.Time                     `json:"startedAt"`
	FinishedAt    *time.Time                    `json:"finishedAt,omitempty"`
	Outcome       domain.WorkflowAttemptOutcome `json:"outcome,omitempty" enum:"succeeded,failed,cancelled"`
	ErrorClass    domain.WorkflowErrorClass     `json:"errorClass,omitempty" enum:"rate_limited,auth,transient,tool,test_failed,review_changes_requested,session_create_failed,agent_start_failed,prompt_delivery_failed,runtime_failed,worker_terminated_unexpectedly,ambiguous_worker_state,reviewer_launch_failed,fix_budget_exhausted,verify_command_failed,verify_timeout,verify_environment_error,verify_artifact_missing,verify_artifact_mismatch,verify_workspace_changed,verify_ambiguous,capacity_exhausted,binary_missing,provider_auth_required,provider_workspace_trust_required,provider_preflight_failed"`
	RetryAfter    *time.Time                    `json:"retryAfter,omitempty"`
}

// WorkflowStepView is one step in a workflow run, with its recorded attempts
// and the facts from its latest durable checkpoint (Checkpoint 8B), when any.
type WorkflowStepView struct {
	ID              string                   `json:"id"`
	Kind            domain.WorkflowStepKind  `json:"kind" enum:"plan,work,review,fix,verify,advance"`
	Ordinal         int64                    `json:"ordinal"`
	DependsOnStepID string                   `json:"dependsOnStepId,omitempty"`
	State           domain.WorkflowStepState `json:"state" enum:"pending,ready,running,waiting,completed,failed,cancelled"`
	AssignedHarness string                   `json:"assignedHarness,omitempty"`
	SessionID       string                   `json:"sessionId,omitempty"`
	ReviewRunID     string                   `json:"reviewRunId,omitempty"`
	CreatedAt       time.Time                `json:"createdAt"`
	UpdatedAt       time.Time                `json:"updatedAt"`
	CompletedAt     *time.Time               `json:"completedAt,omitempty"`
	Attempts        []WorkflowAttemptView    `json:"attempts"`
	// Branch, WorktreePath, and HeadSHA come from the step's latest
	// workflow_checkpoint row, when one exists (Checkpoint 8B).
	Branch       string `json:"branch,omitempty"`
	WorktreePath string `json:"worktreePath,omitempty"`
	HeadSHA      string `json:"headSha,omitempty"`
	NextAction   string `json:"nextAction,omitempty"`
	// Reviewer, Verdict, Target, and FindingsSummary surface the review
	// step's live review_run facts (Checkpoint 8C), fetched at read time —
	// never persisted into workflow_checkpoints beyond the review_run_id
	// reference itself.
	Reviewer        string                     `json:"reviewer,omitempty"`
	Verdict         string                     `json:"verdict,omitempty" enum:",approved,changes_requested"`
	Target          string                     `json:"target,omitempty"`
	FindingsSummary string                     `json:"findingsSummary,omitempty"`
	Verification    *workflowcore.VerifyResult `json:"verification,omitempty"`
	// ReviewPolicy surfaces Checkpoint 8I's durable REQUIRED/SKIPPED
	// decision for a review step, independent of whether a reviewer ever
	// actually ran — the frontend uses this to distinguish "Reviewed: No —
	// policy skipped" from "Claude approved" rather than inferring it from
	// the absence of Reviewer/Verdict.
	ReviewPolicy *workflowcore.ReviewPolicyDecision `json:"reviewPolicy,omitempty"`
	// Routing surfaces Checkpoint 8P-C.1's persisted ExecutionRouter
	// decision for this step (worker/reviewer/planner/decision-resolver
	// roles) -- read back verbatim from the routing_decision checkpoint
	// already written at dispatch time, never recomputed for display. Nil
	// for a step kind that never routes (e.g. verify/advance) or one that
	// hasn't dispatched yet.
	Routing *RoutingDecisionView `json:"routing,omitempty"`
	// FixDelivery surfaces the durable evidence for this fix step's newest
	// dispatched cycle — which review verdict authorized it, which findings
	// travelled and whether they were embedded verbatim in the delivered
	// prompt, the attempt/session it is bound to, and what the transport could
	// prove about the submit. Read back verbatim from the dispatch checkpoints;
	// nothing here is recomputed for display and no prompt text is exposed.
	// Nil for a non-fix step or one that has never dispatched.
	FixDelivery *FixDeliveryView `json:"fixDelivery,omitempty"`
}

// FixDeliveryView is the wire shape of workflow.FixDeliveryReport. It is
// deliberately all non-secret: identifiers, counts, sizes, digests and a
// bounded findings snippet no longer than the review step's own
// findingsSummary. The fix prompt itself is never persisted or returned.
type FixDeliveryView struct {
	State        string    `json:"state" enum:"intent_recorded,dispatched,transport_retry,loaded_not_submitted"`
	DispatchedAt time.Time `json:"dispatchedAt"`

	ReviewRunID     string `json:"reviewRunId,omitempty"`
	ReviewVerdict   string `json:"reviewVerdict,omitempty" enum:",approved,changes_requested"`
	ReviewTargetSHA string `json:"reviewTargetSha,omitempty"`

	FindingsSource   string `json:"findingsSource,omitempty" enum:",review_run,verification"`
	FindingsCount    int    `json:"findingsCount"`
	FindingsBytes    int    `json:"findingsBytes"`
	FindingsDigest   string `json:"findingsDigest,omitempty"`
	FindingsEmbedded bool   `json:"findingsEmbedded"`
	FindingsSnippet  string `json:"findingsSnippet,omitempty"`

	CycleNumber      int    `json:"cycleNumber"`
	FixAttemptID     string `json:"fixAttemptId,omitempty"`
	TransportAttempt int    `json:"transportAttempt,omitempty"`
	SessionID        string `json:"sessionId,omitempty"`

	PromptBytes   int    `json:"promptBytes"`
	Transport     string `json:"transport,omitempty"`
	ContextPack   bool   `json:"contextPack"`
	PromptReceipt string `json:"promptReceipt,omitempty"`
	Submission    string `json:"submission,omitempty"`
	Acknowledged  bool   `json:"acknowledged"`
	ReceiptMatch  string `json:"receiptMatch,omitempty" enum:",match,other,none"`

	TerminalErrorClass string `json:"terminalErrorClass,omitempty"`
	TerminalOutcome    string `json:"terminalOutcome,omitempty"`
	NextAction         string `json:"nextAction,omitempty"`
	Reason             string `json:"reason,omitempty"`
}

func fixDeliveryView(r *workflowcore.FixDeliveryReport) *FixDeliveryView {
	if r == nil {
		return nil
	}
	return &FixDeliveryView{
		State:              string(r.State),
		DispatchedAt:       r.DispatchedAt,
		ReviewRunID:        r.ReviewRunID,
		ReviewVerdict:      r.ReviewVerdict,
		ReviewTargetSHA:    r.ReviewTargetSHA,
		FindingsSource:     r.FindingsSource,
		FindingsCount:      r.FindingsCount,
		FindingsBytes:      r.FindingsBytes,
		FindingsDigest:     r.FindingsDigest,
		FindingsEmbedded:   r.FindingsEmbedded,
		FindingsSnippet:    r.FindingsSnippet,
		CycleNumber:        r.CycleNumber,
		FixAttemptID:       r.FixAttemptID,
		TransportAttempt:   r.TransportAttempt,
		SessionID:          r.SessionID,
		PromptBytes:        r.PromptBytes,
		Transport:          string(r.Transport),
		ContextPack:        r.ContextPack,
		PromptReceipt:      r.PromptReceipt,
		Submission:         string(r.Submission),
		Acknowledged:       r.Acknowledged,
		ReceiptMatch:       r.ReceiptMatch,
		TerminalErrorClass: string(r.TerminalErrorClass),
		TerminalOutcome:    string(r.TerminalOutcome),
		NextAction:         r.NextAction,
		Reason:             r.Reason,
	}
}

// RoutingProfileView is safe, display-only metadata for a provider profile
// referenced by a routing decision (Checkpoint 8P-C.1 §15) -- never a
// runtime-home path, credential, or secret ciphertext. Nil when the
// decision predates profile-level routing (harness-only) or the profile
// could no longer be resolved for the run's owner.
type RoutingProfileView struct {
	ID          string `json:"id"`
	Provider    string `json:"provider"`
	Harness     string `json:"harness"`
	DisplayName string `json:"displayName"`
	Model       string `json:"model,omitempty"`
}

// RoutingDecisionView is the wire shape of a domain.RoutingDecision
// (Checkpoint 8P-C.1 §14). Only fields actually persisted/derivable are
// populated -- an unknown value stays omitted, never fabricated.
type RoutingDecisionView struct {
	Role              string               `json:"role"`
	Complexity        string               `json:"complexity,omitempty"`
	PreferredHarness  string               `json:"preferredHarness,omitempty"`
	SelectedHarness   string               `json:"selectedHarness,omitempty"`
	PreferredProfile  *RoutingProfileView  `json:"preferredProfile,omitempty"`
	SelectedProfile   *RoutingProfileView  `json:"selectedProfile,omitempty"`
	FallbackProfiles  []RoutingProfileView `json:"fallbackProfiles,omitempty"`
	FallbackUsed      bool                 `json:"fallbackUsed"`
	Waiting           bool                 `json:"waiting"`
	ReasonCodes       []string             `json:"reasonCodes,omitempty"`
	PolicyVersion     string               `json:"policyVersion,omitempty"`
	CapacityByProfile map[string]string    `json:"capacityByProfile,omitempty"`
}

func routingDecisionView(d domain.RoutingDecision, profiles map[domain.ProviderProfileID]domain.ProviderProfile) *RoutingDecisionView {
	view := &RoutingDecisionView{
		Role:             string(d.Role),
		Complexity:       d.Complexity,
		PreferredHarness: string(d.PreferredHarness),
		SelectedHarness:  string(d.SelectedHarness),
		Waiting:          d.Waiting,
		PolicyVersion:    d.PolicyVersion,
	}
	for _, r := range d.ReasonCodes {
		view.ReasonCodes = append(view.ReasonCodes, string(r))
	}
	if d.PreferredProfileID != "" {
		view.PreferredProfile = routingProfileView(d.PreferredProfileID, profiles)
	}
	if d.SelectedProfileID != "" {
		view.SelectedProfile = routingProfileView(d.SelectedProfileID, profiles)
		view.FallbackUsed = d.PreferredProfileID != "" && d.SelectedProfileID != d.PreferredProfileID
	}
	for _, id := range d.FallbackProfileOrder {
		if p := routingProfileView(id, profiles); p != nil {
			view.FallbackProfiles = append(view.FallbackProfiles, *p)
		}
	}
	if len(d.CapacityStateByProfile) > 0 {
		view.CapacityByProfile = make(map[string]string, len(d.CapacityStateByProfile))
		for id, state := range d.CapacityStateByProfile {
			view.CapacityByProfile[string(id)] = string(state)
		}
	}
	return view
}

func routingProfileView(id domain.ProviderProfileID, profiles map[domain.ProviderProfileID]domain.ProviderProfile) *RoutingProfileView {
	p, ok := profiles[id]
	if !ok {
		return nil
	}
	return &RoutingProfileView{
		ID: string(p.ID), Provider: p.Provider, Harness: string(p.Harness),
		DisplayName: p.DisplayName, Model: p.DefaultModel,
	}
}

// WorkflowRunView is a workflow run summary (no step/attempt fan-out).
type WorkflowRunView struct {
	ID          string                  `json:"id"`
	ProjectID   string                  `json:"projectId"`
	Objective   string                  `json:"objective"`
	State       domain.WorkflowRunState `json:"state" enum:"pending,running,waiting,needs_attention,completed,failed,cancelled"`
	CreatedAt   time.Time               `json:"createdAt"`
	UpdatedAt   time.Time               `json:"updatedAt"`
	CompletedAt *time.Time              `json:"completedAt,omitempty"`
	CancelledAt *time.Time              `json:"cancelledAt,omitempty"`
	// Origin marks a run AO created for itself rather than one the project
	// asked for. Today the only value is "incident_repair" — Checkpoint
	// 8P-E.21 — and it exists so the Board can group AO's automatic repairs
	// away from the project's ordinary work instead of interleaving them, and
	// so the run page can say what this run is and link back to the incident
	// that authorised it. Empty for every ordinary run.
	Origin *WorkflowRunOriginView `json:"origin,omitempty"`
	// ArchivedAt is set once a human has cancelled and archived the run. It is
	// a presentation fact, not an execution one: an archived run is absent from
	// the active Board and present in the archived view, and every one of its
	// durable rows is retained.
	ArchivedAt *time.Time `json:"archivedAt,omitempty"`
	// NextAction is the run's last-known next action across all its steps'
	// checkpoints (e.g. "start_review"), informational only (Checkpoint 8B).
	NextAction string `json:"nextAction,omitempty"`
	// NextWakeAt/WaitReason/WakeAttemptCount are Checkpoint 8N.1's minimal
	// telemetry surface for a durable capacity wait: the soonest scheduled
	// automatic retry, why the run is waiting, and how many times this exact
	// wait has already retried. All zero-value (NextWakeAt nil, WaitReason
	// "", WakeAttemptCount 0) when no wake is currently open for this run —
	// never a fabricated estimate.
	NextWakeAt       *time.Time `json:"nextWakeAt,omitempty"`
	WaitReason       string     `json:"waitReason,omitempty"`
	WakeAttemptCount int64      `json:"wakeAttemptCount,omitempty"`
	// ExecutionMode is Checkpoint 8P-D's read-only surface of this run's
	// frozen execution policy snapshot: "autonomous" or "manual", decided at
	// run creation time and never re-derived from a later Settings change.
	ExecutionMode string `json:"executionMode" enum:"autonomous,manual"`
	// BranchWait is Checkpoint 8P-E.11's structured waiting_for_branch state
	// for a direct-branch run queued behind another workflow. Present only
	// while the run is genuinely waiting on a branch, so the board renders a
	// real wait or nothing -- never a fabricated "inactive".
	BranchWait *WorkflowBranchWaitView `json:"branchWait,omitempty"`
	// Phase, Attention, AttentionReason and AttentionAction are Checkpoint
	// 8P-E.12's derived lifecycle projection, computed by the same
	// workflow.DeriveLifecycle the project Board uses so the two surfaces can
	// never disagree about what a run is doing. Phase is the mapping
	// document's vocabulary; Attention separates a problem AO is still
	// handling itself from one that genuinely needs the user.
	Phase           string `json:"phase" enum:"queued,planning,running,reviewing,fixing,verifying,waiting,waiting_for_capacity,retrying,blocked,needs_attention,completed,failed,cancelled"`
	Attention       string `json:"attention,omitempty" enum:"ao_internal,human_decision"`
	AttentionReason string `json:"attentionReason,omitempty"`
	AttentionAction string `json:"attentionAction,omitempty"`
	// LastActivityAt is the workflow's own newest durable timestamp — never the
	// worker session's activity state, because an idle worker during a review
	// is not an idle workflow.
	LastActivityAt time.Time `json:"lastActivityAt"`
	// CanContinue is the backend's authoritative answer to "would POST
	// /continue advance this run". The UI renders its Continue/Reanudar control
	// from this flag alone and must never re-derive the rule from state/phase
	// strings — see workflow.canContinueRun for the rules and why they live
	// there.
	CanContinue bool `json:"canContinue"`
	// AttentionWorkflowID names the run a person should actually act on when
	// this run's stop merely mirrors another's (an objective reflecting the task
	// that stopped). Empty for a stop the run owns itself.
	AttentionWorkflowID string `json:"attentionWorkflowId,omitempty"`
	// CapacityWait is the normalized provider-capacity wait projection: which
	// role is blocked, the normalized reason, when the next automatic attempt
	// is, whether a real provider reset is known, and each candidate provider's
	// health evidence with its age. Present only while the run is genuinely
	// parked on capacity.
	CapacityWait *WorkflowCapacityWaitView `json:"capacityWait,omitempty"`
}

// WorkflowCapacityWaitView is the wire form of workflow.CapacityWait.
type WorkflowCapacityWaitView struct {
	Role string `json:"role,omitempty"`
	// Reason is the normalized cause, not a wake reason: provider_health_stale
	// (a past failure whose window expired — AO is re-probing),
	// provider_cooldown, provider_unavailable, or no_eligible_provider.
	Reason string `json:"reason" enum:"provider_health_stale,provider_cooldown,provider_unavailable,no_eligible_provider"`
	// IndependenceRequired records that review independence additionally forbids
	// falling back to the implementer's own provider, however available it is.
	IndependenceRequired bool `json:"independenceRequired,omitempty"`
	// NextAttemptAt is the soonest scheduled automatic retry; KnownResetAt is
	// non-nil only when a provider actually reported its own reset time.
	NextAttemptAt *time.Time `json:"nextAttemptAt,omitempty"`
	KnownResetAt  *time.Time `json:"knownResetAt,omitempty"`
	Attempt       int64      `json:"attempt,omitempty"`
	// Probing is true when AO is actively re-evaluating a blocked provider
	// rather than waiting out a clock.
	Probing   bool                               `json:"probing,omitempty"`
	Providers []WorkflowCapacityWaitProviderView `json:"providers,omitempty"`
}

// WorkflowCapacityWaitProviderView is one candidate provider's capacity
// evidence, including how old the observation behind it is.
type WorkflowCapacityWaitProviderView struct {
	ProfileID   string `json:"profileId"`
	Provider    string `json:"provider,omitempty"`
	Harness     string `json:"harness,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	Capacity    string `json:"capacity" enum:"available,limited,cooldown,unavailable,unknown"`
	HealthState string `json:"healthState,omitempty" enum:"available,cooldown,unavailable,unknown"`
	// HealthReason and FailureClass are the durable observation's own words —
	// e.g. "agent_start_failed (unknown)" — never re-worded here.
	HealthReason string `json:"healthReason,omitempty"`
	FailureClass string `json:"failureClass,omitempty"`
	// ObservedAt is when that observation was recorded; HealthAgeSeconds is its
	// age at response time, so the UI never has to compute a duration against a
	// clock that may not match the daemon's.
	ObservedAt       *time.Time `json:"observedAt,omitempty"`
	HealthAgeSeconds int64      `json:"healthAgeSeconds,omitempty"`
	// Recovery is how this state can clear: cooldown (time-boxed), probe (a
	// capacity probe can prove it), or manual (a human must change credentials
	// or configuration).
	Recovery      string     `json:"recovery,omitempty" enum:"cooldown,probe,manual"`
	CooldownUntil *time.Time `json:"cooldownUntil,omitempty"`
	ProbeEligible bool       `json:"probeEligible,omitempty"`
}

func workflowCapacityWaitView(w *workflowcore.CapacityWait, now time.Time) *WorkflowCapacityWaitView {
	if w == nil {
		return nil
	}
	view := &WorkflowCapacityWaitView{
		Role:                 string(w.Role),
		Reason:               string(w.Reason),
		IndependenceRequired: w.IndependenceRequired,
		NextAttemptAt:        w.NextAttemptAt,
		KnownResetAt:         w.KnownResetAt,
		Attempt:              w.Attempt,
		Probing:              w.Probing,
	}
	for _, p := range w.Providers {
		entry := WorkflowCapacityWaitProviderView{
			ProfileID: string(p.ProfileID), Provider: p.Provider, Harness: string(p.Harness),
			DisplayName: p.DisplayName, Capacity: string(p.Capacity),
			HealthState: string(p.HealthState), HealthReason: p.HealthReason,
			FailureClass: string(p.FailureClass), ObservedAt: p.ObservedAt,
			Recovery: string(p.Recovery), CooldownUntil: p.CooldownUntil,
			ProbeEligible: p.ProbeEligible,
		}
		if p.ObservedAt != nil {
			if age := now.Sub(*p.ObservedAt); age > 0 {
				entry.HealthAgeSeconds = int64(age.Seconds())
			}
		}
		view.Providers = append(view.Providers, entry)
	}
	return view
}

// WorkflowBranchWaitView names the branch a run is queued on and the workflow
// that currently owns it.
type WorkflowBranchWaitView struct {
	Branch              string `json:"branch"`
	RepoPath            string `json:"repoPath,omitempty"`
	HeldByWorkflowRunID string `json:"heldByWorkflowRunId,omitempty"`
	HeldBySessionID     string `json:"heldBySessionId,omitempty"`
	// HeldByState, HeldByReason and AutoResume are Checkpoint 8P-E.13A's
	// resolved-at-read-time answer to "is this queue moving?". AutoResume=false
	// means the branch is held by a workflow that has stopped for a human
	// decision and is protecting uncommitted work — the one case where a queued
	// run genuinely needs someone to act on the OTHER workflow.
	HeldByState  string `json:"heldByState,omitempty" enum:"pending,running,waiting,needs_attention,completed,failed,cancelled"`
	HeldByReason string `json:"heldByReason,omitempty"`
	AutoResume   bool   `json:"autoResume,omitempty"`
}

func workflowBranchWaitView(w *workflowcore.BranchWait) *WorkflowBranchWaitView {
	if w == nil {
		return nil
	}
	return &WorkflowBranchWaitView{
		Branch:              w.Branch,
		RepoPath:            w.RepoPath,
		HeldByWorkflowRunID: w.HeldByWorkflowRunID,
		HeldBySessionID:     w.HeldBySessionID,
		HeldByState:         w.HeldByState,
		HeldByReason:        w.HeldByReason,
		AutoResume:          w.AutoResume,
	}
}

// WorkflowRunDetailView is a workflow run plus its steps and their attempts.
type WorkflowRunDetailView struct {
	Run   WorkflowRunView    `json:"run"`
	Steps []WorkflowStepView `json:"steps"`
	Plan  *WorkflowPlanView  `json:"plan,omitempty"`
	Tasks []WorkflowTaskView `json:"tasks,omitempty"`
	// Usage is Checkpoint 8J's usage/telemetry/session-refresh section. Nil
	// only when the controller has no UsageReader wired (headless/test
	// configurations without a usage summary service) — otherwise always
	// present, with per-field null/"unknown" markers rather than omission.
	Usage *WorkflowUsageResponse `json:"usage,omitempty"`
	// Questions is Checkpoint 8K-A's durable question list (any state, most
	// recent last) — omitted entirely when the controller has no
	// QuestionsReader wired, empty (not omitted) when wired but the run has
	// never had a question.
	Questions []WorkflowQuestionResponse `json:"questions,omitempty"`
	// IntegrationState is Checkpoint 8M.1's git integration summary — present
	// only for master runs (Plan != nil).
	IntegrationState *WorkflowIntegrationStateView `json:"integrationState,omitempty"`
}

// WorkflowIntegrationStateView is Checkpoint 8M.1's read-only surface of
// workflowcore.MasterIntegrationSummary.
type WorkflowIntegrationStateView struct {
	RefName         string `json:"refName"`
	CurrentSHA      string `json:"currentSha,omitempty"`
	TasksIntegrated int    `json:"tasksIntegrated"`
	LatestTaskID    string `json:"latestTaskId,omitempty"`
	Status          string `json:"status"`
	ErrorClass      string `json:"errorClass,omitempty"`
}

type WorkflowPlanView struct {
	Status               domain.WorkflowPlanStatus       `json:"status"`
	ApprovalMode         domain.WorkflowPlanApprovalMode `json:"approvalMode"`
	Provider             string                          `json:"provider,omitempty"`
	Model                string                          `json:"model,omitempty"`
	PromptContextVersion string                          `json:"promptContextVersion"`
	PlanHash             string                          `json:"planHash,omitempty"`
	ErrorClass           string                          `json:"errorClass,omitempty"`
	Generated            *workflowcore.MasterPlan        `json:"generated,omitempty"`
	Validation           *workflowcore.PlanValidation    `json:"validation,omitempty"`
}

type WorkflowTaskView struct {
	ID                  string                        `json:"id"`
	Number              int64                         `json:"number"`
	Title               string                        `json:"title"`
	Description         string                        `json:"description"`
	Dependencies        []string                      `json:"dependencies"`
	AcceptanceCriteria  []string                      `json:"acceptanceCriteria"`
	Verify              workflowcore.VerificationPlan `json:"verify"`
	State               domain.WorkflowTaskState      `json:"state"`
	ExecutionWorkflowID string                        `json:"executionWorkflowId,omitempty"`
	// Planner is everything the plan decided about this task and everything
	// that has happened to it since: execution strategy, dependency and
	// integration ordering, waiting reason, dispatch wave, probable write
	// scope, AO worktree/branch, and integration state. Absent only for a run
	// with no planner projection at all.
	Planner *WorkflowTaskPlannerView `json:"planner,omitempty"`
}

// WorkflowRunResponse is the body of create/get/cancel (200/201).
type WorkflowRunResponse struct {
	Workflow WorkflowRunDetailView `json:"workflow"`
}

// ListWorkflowsResponse is the body of GET /api/v1/workflows.
type ListWorkflowsResponse struct {
	Workflows []WorkflowRunView `json:"workflows"`
}

func workflowRunView(run domain.WorkflowRun, nextAction string) WorkflowRunView {
	return WorkflowRunView{
		ID:            run.ID,
		ProjectID:     run.ProjectID,
		Objective:     run.Objective,
		State:         run.State,
		CreatedAt:     run.CreatedAt,
		UpdatedAt:     run.UpdatedAt,
		CompletedAt:   run.CompletedAt,
		CancelledAt:   run.CancelledAt,
		ArchivedAt:    run.ArchivedAt,
		NextAction:    nextAction,
		ExecutionMode: executionModeForRun(run),
	}
}

// executionModeForRun decodes a run's durable policy_snapshot to surface
// Checkpoint 8P-D's frozen AutonomousMode flag as a stable "autonomous"/
// "manual" string, without exposing the rest of the internal policy
// snapshot shape over the API. Mirrors workflowcore's own
// policyForRun/DefaultWorkflowPolicy fallback: an empty/unparseable/pre-8P-C
// snapshot defaults to "manual", matching the safe-default requirement that
// AutonomousMode's own zero value is false.
func executionModeForRun(run domain.WorkflowRun) string {
	if run.PolicySnapshot != "" && run.PolicySnapshot != "{}" {
		var p domain.WorkflowPolicy
		if err := json.Unmarshal([]byte(run.PolicySnapshot), &p); err == nil && p.Execution.AutonomousMode {
			return "autonomous"
		}
	}
	return "manual"
}

func (c *WorkflowsController) workflowRunDetailView(ctx context.Context, detail workflowcore.RunDetail) WorkflowRunDetailView {
	// Checkpoint 8P-C.1: resolve the run owner's provider profiles ONCE
	// (never per-step) for routing-decision display metadata. Unresolvable
	// (no Ownership/ProviderProfiles wired, run unowned, or the lookup
	// fails) simply means every RoutingDecisionView surfaces harness/reason
	// data with no *Profile display fields -- never blocks the response.
	var ownedProfiles map[domain.ProviderProfileID]domain.ProviderProfile
	if c.Ownership != nil && c.ProviderProfiles != nil {
		if owner, err := c.Ownership.GetWorkflowRunOwner(ctx, detail.Run.ID); err == nil && owner != nil {
			if profiles, err := c.ProviderProfiles.List(ctx, *owner); err == nil {
				ownedProfiles = make(map[domain.ProviderProfileID]domain.ProviderProfile, len(profiles))
				for _, p := range profiles {
					ownedProfiles[p.ID] = p
				}
			}
		}
	}
	steps := make([]WorkflowStepView, 0, len(detail.Steps))
	for _, sd := range detail.Steps {
		step := sd.Step
		dependsOn := ""
		if step.DependsOnStepID != nil {
			dependsOn = *step.DependsOnStepID
		}
		sessionID := ""
		if step.SessionID != nil {
			sessionID = *step.SessionID
		}
		reviewRunID := ""
		if step.ReviewRunID != nil {
			reviewRunID = *step.ReviewRunID
		}
		attempts := make([]WorkflowAttemptView, 0, len(sd.Attempts))
		for _, a := range sd.Attempts {
			attempts = append(attempts, WorkflowAttemptView{
				ID:            a.ID,
				AttemptNumber: a.AttemptNumber,
				Harness:       a.Harness,
				Model:         a.Model,
				StartedAt:     a.StartedAt,
				FinishedAt:    a.FinishedAt,
				Outcome:       a.Outcome,
				ErrorClass:    a.ErrorClass,
				RetryAfter:    a.RetryAfter,
			})
		}
		var branch, worktreePath, headSHA, nextAction string
		var verification *workflowcore.VerifyResult
		if sd.LatestCheckpoint != nil {
			branch = sd.LatestCheckpoint.Branch
			worktreePath = sd.LatestCheckpoint.WorktreePath
			headSHA = sd.LatestCheckpoint.HeadSHA
			nextAction = sd.LatestCheckpoint.NextAction
			if step.Kind == domain.WorkflowStepVerify && sd.LatestCheckpoint.DurablePhase == "verify_result" {
				var result workflowcore.VerifyResult
				if json.Unmarshal([]byte(sd.LatestCheckpoint.RetryState), &result) == nil {
					verification = &result
				}
			}
		}
		var reviewer, verdict, target, findings string
		if sd.Review != nil {
			reviewer = string(sd.Review.Harness)
			verdict = string(sd.Review.Verdict)
			target = sd.Review.Target
			findings = sd.Review.FindingsSummary
		}
		var routing *RoutingDecisionView
		if sd.Routing != nil {
			routing = routingDecisionView(*sd.Routing, ownedProfiles)
		}
		steps = append(steps, WorkflowStepView{
			ID:              step.ID,
			Kind:            step.Kind,
			Ordinal:         step.Ordinal,
			DependsOnStepID: dependsOn,
			State:           step.State,
			AssignedHarness: step.AssignedHarness,
			SessionID:       sessionID,
			ReviewRunID:     reviewRunID,
			CreatedAt:       step.CreatedAt,
			UpdatedAt:       step.UpdatedAt,
			CompletedAt:     step.CompletedAt,
			Attempts:        attempts,
			Branch:          branch,
			WorktreePath:    worktreePath,
			HeadSHA:         headSHA,
			NextAction:      nextAction,
			Reviewer:        reviewer,
			Verdict:         verdict,
			Target:          target,
			FindingsSummary: findings,
			Verification:    verification,
			ReviewPolicy:    sd.ReviewPolicy,
			Routing:         routing,
			FixDelivery:     fixDeliveryView(sd.FixDelivery),
		})
	}
	runView := workflowRunView(detail.Run, detail.NextAction)
	runView.NextWakeAt = detail.NextWakeAt
	runView.WaitReason = detail.WaitReason
	runView.WakeAttemptCount = detail.WakeAttemptCount
	runView.BranchWait = workflowBranchWaitView(detail.BranchWait)
	// Checkpoint 8P-E.12: one derivation, two surfaces. The Board and this
	// detail view both read workflow.DeriveLifecycle, so a run can never be
	// "Reviewing" on one screen and "Inactive" on the other.
	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail, Questions: detail.Questions})
	runView.Phase = string(life.Phase)
	runView.Attention = string(life.Attention)
	runView.AttentionReason = life.AttentionReason
	runView.AttentionAction = life.AttentionAction
	runView.LastActivityAt = life.LastActivityAt
	runView.CanContinue = life.CanContinue
	// Checkpoint 8P-E.21: label AO's own repair runs so neither the Board nor a
	// person mistakes one for work the project asked for.
	if adv, ok := c.incidentAdvisor(); ok {
		if origin, isRepair := adv.RepairOriginFor(ctx, detail.Run.ID); isRepair {
			runView.Origin = &WorkflowRunOriginView{
				Kind: origin.Origin, IncidentID: origin.IncidentID,
				SourceWorkflowID: origin.SourceRunID, ApprovedBy: origin.ApprovedBy,
			}
		}
	}
	runView.AttentionWorkflowID = life.AttentionWorkflowID
	runView.CapacityWait = workflowCapacityWaitView(detail.CapacityWait, time.Now().UTC())
	view := WorkflowRunDetailView{Run: runView, Steps: steps}
	if detail.Plan != nil {
		pv := WorkflowPlanView{Status: detail.Plan.Status, ApprovalMode: detail.Plan.ApprovalMode, Provider: detail.Plan.Provider, Model: detail.Plan.Model, PromptContextVersion: detail.Plan.PromptContextVersion, PlanHash: detail.Plan.PlanHash, ErrorClass: detail.Plan.ErrorClass}
		var generated workflowcore.MasterPlan
		if detail.Plan.GeneratedPlanJSON != "" && detail.Plan.GeneratedPlanJSON != "{}" && json.Unmarshal([]byte(detail.Plan.GeneratedPlanJSON), &generated) == nil {
			pv.Generated = &generated
		}
		var validation workflowcore.PlanValidation
		if detail.Plan.ValidationJSON != "" && detail.Plan.ValidationJSON != "{}" && json.Unmarshal([]byte(detail.Plan.ValidationJSON), &validation) == nil {
			pv.Validation = &validation
		}
		view.Plan = &pv
	}
	if detail.IntegrationState != nil {
		view.IntegrationState = &WorkflowIntegrationStateView{
			RefName:         detail.IntegrationState.RefName,
			CurrentSHA:      detail.IntegrationState.CurrentSHA,
			TasksIntegrated: detail.IntegrationState.TasksIntegrated,
			LatestTaskID:    detail.IntegrationState.LatestTaskID,
			Status:          detail.IntegrationState.Status,
			ErrorClass:      detail.IntegrationState.ErrorClass,
		}
	}
	planIDByTask := map[string]string{}
	for _, task := range detail.Tasks {
		planIDByTask[task.ID] = task.PlanStepID
	}
	planner := workflowTaskPlannerViews(detail.TaskPlanner)
	for _, task := range detail.Tasks {
		var criteria []string
		var verify workflowcore.VerificationPlan
		_ = json.Unmarshal([]byte(task.AcceptanceCriteriaJSON), &criteria)
		_ = json.Unmarshal([]byte(task.VerifyJSON), &verify)
		deps := make([]string, 0, len(task.Dependencies))
		for _, dep := range task.Dependencies {
			if id := planIDByTask[dep]; id != "" {
				deps = append(deps, id)
			}
		}
		execID := ""
		if task.ExecutionRunID != nil {
			execID = *task.ExecutionRunID
		}
		view.Tasks = append(view.Tasks, WorkflowTaskView{ID: task.PlanStepID, Number: task.Ordinal, Title: task.Title, Description: task.Description, Dependencies: deps, AcceptanceCriteria: criteria, Verify: verify, State: task.State, ExecutionWorkflowID: execID, Planner: workflowTaskPlannerView(planner[task.ID], planIDByTask)})
	}
	var questionsForRun []domain.WorkflowQuestion
	if c.QuestionsReader != nil {
		if qs, err := c.QuestionsReader.ListByRun(ctx, detail.Run.ID); err == nil {
			questionsForRun = qs
			view.Questions = enrichedQuestionResponses(ctx, c.QuestionsReader, qs)
		}
	}
	if c.UsageReader != nil {
		usageView := BuildWorkflowUsageView(ctx, detail, c.UsageReader)
		if c.QuestionsReader != nil {
			// Checkpoint 8K-B pass 3: Decisions telemetry is derived
			// read-time from the same questions list plus every resolution
			// attempt ever recorded for this run — no new store reads
			// beyond what QuestionsReader already exposes.
			if resolutions, err := c.QuestionsReader.ListResolutionsByRun(ctx, detail.Run.ID); err == nil {
				usageView.Decisions = BuildDecisionsUsageView(questionsForRun, resolutions)
			}
		}
		usage := workflowUsageResponse(usageView)
		view.Usage = &usage
	}
	return view
}

// WorkflowRunOriginView explains a run AO created for itself.
type WorkflowRunOriginView struct {
	// Kind is the closed vocabulary of AO-created runs. "incident_repair" is
	// currently the only member.
	Kind string `json:"kind"`
	// IncidentID and SourceWorkflowID are what this run is a repair OF and
	// where it came from, so a reader is never stranded in an unexplained run.
	IncidentID       string `json:"incidentId,omitempty"`
	SourceWorkflowID string `json:"sourceWorkflowId,omitempty"`
	ApprovedBy       string `json:"approvedBy,omitempty"`
}

// WorkflowsController owns the /workflows routes. A nil Svc returns 501.
type WorkflowsController struct {
	Svc workflowsvc.Manager
	// UsageReader backs Checkpoint 8J's Usage section. Optional: nil leaves
	// WorkflowRunDetailView.Usage unset rather than failing the request, so
	// a headless/test daemon without usage wiring keeps working unchanged.
	UsageReader SessionUsageLookup
	// QuestionsReader backs Checkpoint 8K-A's Questions section, embedded
	// into every run-detail response the same way UsageReader is. Optional:
	// nil leaves WorkflowRunDetailView.Questions unset.
	QuestionsReader WorkflowQuestionsService
	// Ownership backs Checkpoint 8P-A's ownership scoping. Nil preserves
	// pre-8P-A unscoped behavior exactly.
	Ownership WorkflowOwnershipStore
	// TrustedLocal mirrors config.Config.TrustedLocalMode; scoping is only
	// enforced when this is false — see ProjectsController's own field for
	// the identical reasoning.
	TrustedLocal bool
	// ProviderProfiles backs Checkpoint 8P-C.1's routing-decision profile
	// display metadata (safe fields only -- see RoutingProfileView). Nil
	// leaves every StepDetail.Routing.*Profile field unset; the raw
	// harness/reason-code fields still surface.
	ProviderProfiles providerprofilesvc.Manager
}

func (c *WorkflowsController) scopingEnforced() bool {
	return !c.TrustedLocal && c.Ownership != nil
}

func (c *WorkflowsController) stampOwner(r *http.Request, id string, autonomousOverride *bool) {
	if c.Ownership == nil {
		return
	}
	user, ok := identity.FromContext(r.Context())
	if !ok {
		return
	}
	_, _ = c.Ownership.SetWorkflowRunOwner(r.Context(), id, user.ID)
	// Checkpoint 8P-C: embed the caller's execution policy into the
	// just-created run's policy snapshot, using the same resolved identity
	// stampOwner just used -- never a second identity lookup. Optional
	// (type-asserted): a Svc predating 8P-C simply skips this, leaving the
	// run on its default policy_snapshot exactly as before. Checkpoint
	// 8P-D.1: autonomousOverride carries the create request's explicit
	// per-run Manual/Autonomous choice (nil when the request omitted it, in
	// which case the caller's stored/default policy applies unchanged).
	if applier, ok := c.Svc.(workflowsvc.ExecutionPolicyApplier); ok {
		_ = applier.ApplyExecutionPolicySnapshot(r.Context(), id, user.ID, autonomousOverride)
	}
}

func (c *WorkflowsController) runVisible(ctx context.Context, id string, current domain.UserID) bool {
	owner, err := c.Ownership.GetWorkflowRunOwner(ctx, id)
	if err != nil {
		return false
	}
	return !(owner != nil && *owner != current)
}

// Register mounts the workflow routes on the supplied router.
func (c *WorkflowsController) Register(r chi.Router) {
	r.Post("/projects/{projectId}/workflows", c.create)
	r.Get("/projects/{projectId}/board", c.board)
	r.Get("/workflows/{workflowId}", c.get)
	r.Get("/workflows", c.list)
	r.Post("/workflows/{workflowId}/cancel", c.cancel)
	r.Post("/workflows/{workflowId}/cancel-archive", c.cancelAndArchive)
	r.Get("/projects/{projectId}/board/history", c.boardHistory)
	r.Post("/workflows/{workflowId}/start", c.start)
	r.Post("/workflows/{workflowId}/continue", c.continueRun)
	r.Post("/workflows/{workflowId}/tasks/{taskId}/resume", c.resumeTask)
	r.Post("/workflows/{workflowId}/tasks/{taskId}/fresh-review-exception", c.authorizeFreshReviewException)
	r.Post("/workflows/{workflowId}/tasks/{taskId}/criteria/amend", c.amendTaskCriterion)
	r.Post("/workflows/{workflowId}/tasks/{taskId}/criteria/resume-review", c.resumeAmendedTaskReview)

	// Checkpoint 8P-E.18 — the Incident Advisor behind the "¿Qué hago?" control.
	// Four routes rather than one, because the split IS the authorization
	// model: proposing and executing must not be reachable through the same
	// capability. See workflow_incident.go.
	r.Get("/workflows/{workflowId}/incident", c.getIncident)
	r.Post("/workflows/{workflowId}/incident/diagnose", c.diagnoseIncident)
	r.Post("/workflows/{workflowId}/incident/diagnosis", c.submitIncidentDiagnosis)
	r.Post("/workflows/{workflowId}/incident/execute", c.executeIncidentAction)
	r.Post("/workflows/{workflowId}/plan/generate", c.generatePlan)
	r.Get("/workflows/{workflowId}/plan", c.get)
	r.Post("/workflows/{workflowId}/plan/approve", c.approvePlan)
	r.Post("/workflows/{workflowId}/plan/reject", c.rejectPlan)
	(&WorkflowQuestionsController{Svc: c.QuestionsReader, Ownership: c.Ownership, TrustedLocal: c.TrustedLocal}).Register(r)
}

func (c *WorkflowsController) create(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/projects/{projectId}/workflows")
		return
	}
	projectID := strings.TrimSpace(chi.URLParam(r, "projectId"))
	var in CreateWorkflowRunRequest
	if err := decodeJSON(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	verification := workflowcore.VerificationPlan{}
	for _, check := range in.Verification.Commands {
		verification.Commands = append(verification.Commands, workflowcore.VerificationCommandCheck{Command: check.Command, Args: check.Args, WorkingDirectory: check.WorkingDirectory, TimeoutSeconds: check.TimeoutSeconds, RequiredExitCode: check.RequiredExitCode, RetrySafe: check.RetrySafe})
	}
	for _, check := range in.Verification.Files {
		verification.Files = append(verification.Files, workflowcore.VerificationFileCheck{Path: check.Path, Exists: check.Exists, ExactContent: check.ExactContent, SHA256: check.SHA256})
	}
	var detail workflowcore.RunDetail
	var err error
	if in.MasterPlan {
		plannerSvc, ok := c.Svc.(workflowsvc.PlannerManager)
		if !ok {
			apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/projects/{projectId}/workflows")
			return
		}
		detail, err = plannerSvc.CreateObjectiveRun(r.Context(), projectID, strings.TrimSpace(in.Objective), in.PlanApprovalMode)
	} else {
		detail, err = c.Svc.CreateRun(r.Context(), projectID, strings.TrimSpace(in.Objective), verification)
	}
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	c.stampOwner(r, detail.Run.ID, in.Autonomous)
	// Re-fetch after stampOwner: it just wrote the caller's (possibly
	// per-run-overridden) execution policy into this run's policy_snapshot
	// and may have scheduled an autonomous-kickoff wake -- `detail` above
	// was read before that write, so returning it as-is would hand the
	// client a response whose executionMode/nextWakeAt/waitReason are one
	// write behind the row this same request just produced (caught in
	// Checkpoint 8P-D.1's real create-an-autonomous-run smoke test).
	if refreshed, refreshErr := c.Svc.GetRun(r.Context(), detail.Run.ID); refreshErr == nil {
		detail = refreshed
	}
	envelope.WriteJSON(w, http.StatusCreated, WorkflowRunResponse{Workflow: c.workflowRunDetailView(r.Context(), detail)})
}

func (c *WorkflowsController) generatePlan(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/workflows/{workflowId}/plan/generate")
		return
	}
	plannerSvc, ok := c.Svc.(workflowsvc.PlannerManager)
	if !ok {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/workflows/{workflowId}/plan/generate")
		return
	}
	detail, err := plannerSvc.GeneratePlan(r.Context(), chi.URLParam(r, "workflowId"))
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, WorkflowRunResponse{Workflow: c.workflowRunDetailView(r.Context(), detail)})
}
func (c *WorkflowsController) approvePlan(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/workflows/{workflowId}/plan/approve")
		return
	}
	plannerSvc, ok := c.Svc.(workflowsvc.PlannerManager)
	if !ok {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/workflows/{workflowId}/plan/approve")
		return
	}
	detail, err := plannerSvc.ApprovePlan(r.Context(), chi.URLParam(r, "workflowId"))
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, WorkflowRunResponse{Workflow: c.workflowRunDetailView(r.Context(), detail)})
}
func (c *WorkflowsController) rejectPlan(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/workflows/{workflowId}/plan/reject")
		return
	}
	plannerSvc, ok := c.Svc.(workflowsvc.PlannerManager)
	if !ok {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/workflows/{workflowId}/plan/reject")
		return
	}
	detail, err := plannerSvc.RejectPlan(r.Context(), chi.URLParam(r, "workflowId"))
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, WorkflowRunResponse{Workflow: c.workflowRunDetailView(r.Context(), detail)})
}

func (c *WorkflowsController) get(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/workflows/{workflowId}")
		return
	}
	workflowID := chi.URLParam(r, "workflowId")
	if c.scopingEnforced() {
		user, err := identity.Require(r)
		if err != nil {
			envelope.WriteError(w, r, err)
			return
		}
		if !c.runVisible(r.Context(), workflowID, user.ID) {
			envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "WORKFLOW_NOT_FOUND", "workflow run not found", nil)
			return
		}
	}
	detail, err := c.Svc.GetRun(r.Context(), workflowID)
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, WorkflowRunResponse{Workflow: c.workflowRunDetailView(r.Context(), detail)})
}

func (c *WorkflowsController) list(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/workflows")
		return
	}
	filter := workflowsvc.ListFilter{ProjectID: strings.TrimSpace(r.URL.Query().Get("projectId"))}
	runs, err := c.Svc.ListRuns(r.Context(), filter)
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	if c.scopingEnforced() {
		user, err := identity.Require(r)
		if err != nil {
			envelope.WriteError(w, r, err)
			return
		}
		visible := make([]domain.WorkflowRun, 0, len(runs))
		for _, run := range runs {
			if c.runVisible(r.Context(), run.ID, user.ID) {
				visible = append(visible, run)
			}
		}
		runs = visible
	}
	views := make([]WorkflowRunView, 0, len(runs))
	for _, run := range runs {
		views = append(views, workflowRunView(run, ""))
	}
	envelope.WriteJSON(w, http.StatusOK, ListWorkflowsResponse{Workflows: views})
}

func (c *WorkflowsController) cancel(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/workflows/{workflowId}/cancel")
		return
	}
	detail, err := c.Svc.CancelRun(r.Context(), chi.URLParam(r, "workflowId"))
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, WorkflowRunResponse{Workflow: c.workflowRunDetailView(r.Context(), detail)})
}

// cancelAndArchive stops a workflow (cascading to its children) and moves it
// off the active Board.
//
// Deliberately NOT a DELETE: nothing is removed. The route is a POST on the run
// because it is a lifecycle command with side effects on child runs, branch
// locks and wake schedules, and because retrying it must be safe — the
// underlying operation is idempotent, so a repeated request returns the same
// 200 rather than a 409.
func (c *WorkflowsController) cancelAndArchive(w http.ResponseWriter, r *http.Request) {
	archiver, ok := c.Svc.(workflowsvc.RunArchiver)
	if c.Svc == nil || !ok {
		apispec.NotImplemented(w, r, "POST", "/api/v1/workflows/{workflowId}/cancel-archive")
		return
	}
	detail, err := archiver.CancelAndArchiveRun(r.Context(), chi.URLParam(r, "workflowId"))
	if errors.Is(err, workflowsvc.ErrArchiveUnsupported) {
		// The service exposes the action but its store cannot archive: the
		// same answer as a controller with no archiver wired at all.
		apispec.NotImplemented(w, r, "POST", "/api/v1/workflows/{workflowId}/cancel-archive")
		return
	}
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, WorkflowRunResponse{Workflow: c.workflowRunDetailView(r.Context(), detail)})
}

func (c *WorkflowsController) start(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/workflows/{workflowId}/start")
		return
	}
	detail, err := c.Svc.StartRun(r.Context(), chi.URLParam(r, "workflowId"))
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, WorkflowRunResponse{Workflow: c.workflowRunDetailView(r.Context(), detail)})
}

func (c *WorkflowsController) continueRun(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/workflows/{workflowId}/continue")
		return
	}
	detail, err := c.Svc.ContinueRun(r.Context(), chi.URLParam(r, "workflowId"))
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, WorkflowRunResponse{Workflow: c.workflowRunDetailView(r.Context(), detail)})
}

// resumeTask releases one task parked in needs_attention after a person has
// dealt with what parked it — the explicit human half of the task-level
// attention state.
//
// POST rather than PATCH, and idempotent rather than conditional: resuming a
// task that is not parked returns the run unchanged with 200. A retried request
// or a double-clicked button must never produce a second integration attempt,
// and making that true of the operation itself is more robust than making the
// caller careful.
func (c *WorkflowsController) resumeTask(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/workflows/{workflowId}/tasks/{taskId}/resume")
		return
	}
	detail, err := c.Svc.ResumeTask(r.Context(), chi.URLParam(r, "workflowId"), chi.URLParam(r, "taskId"))
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, WorkflowRunResponse{Workflow: c.workflowRunDetailView(r.Context(), detail)})
}

// AuthorizeFreshReviewExceptionRequest authorizes ONE additional integration
// fresh review for a task whose ordinary budget is spent.
//
// ApprovedBy and Reason are required for the same reason they are on an
// amendment: this widens a guard, and a widening nobody can be asked about
// afterwards is indistinguishable from the guard not being there.
type AuthorizeFreshReviewExceptionRequest struct {
	ApprovedBy string `json:"approvedBy"`
	Reason     string `json:"reason"`
	// Reauthorize authorizes a SECOND grant for a workspace state that already
	// has one. Without it such a request returns the existing grant unchanged.
	Reauthorize bool `json:"reauthorize,omitempty"`
}

func (c *WorkflowsController) authorizeFreshReviewException(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/workflows/{workflowId}/tasks/{taskId}/fresh-review-exception")
		return
	}
	var body AuthorizeFreshReviewExceptionRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeWorkflowError(w, r, fmt.Errorf("%w: malformed request body", workflowcore.ErrInvalid))
		return
	}
	exception, err := c.Svc.AuthorizeIntegrationFreshReviewException(r.Context(), workflowcore.IntegrationFreshReviewExceptionRequest{
		MasterRunID: chi.URLParam(r, "workflowId"),
		TaskID:      chi.URLParam(r, "taskId"),
		ApprovedBy:  body.ApprovedBy,
		Reason:      body.Reason,
		Reauthorize: body.Reauthorize,
	})
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, exception)
}

// AmendTaskCriterionRequest is one human-approved amendment of one acceptance
// criterion (migration 0132).
//
// ApprovedBy is a required field rather than an inferred identity because the
// whole legitimacy of the mechanism rests on a person having said yes to THIS
// change: an amendment attributed to whoever happened to hold the session token
// is an amendment nobody can be asked about afterwards.
type AmendTaskCriterionRequest struct {
	CriterionIndex int `json:"criterionIndex"`
	// OriginalCriterion, when given, must match the text currently at that
	// index — the caller's proof it is amending the criterion it means to.
	OriginalCriterion string `json:"originalCriterion,omitempty"`
	// AmendedCriterion replaces it. Omit or leave empty to declare the
	// criterion obsolete and remove it.
	AmendedCriterion string `json:"amendedCriterion,omitempty"`
	// Reason and Evidence are both required: an amendment must say why the
	// criterion stopped describing reality and offer something checkable.
	Reason     string   `json:"reason"`
	Evidence   []string `json:"evidence"`
	ApprovedBy string   `json:"approvedBy"`
}

// AmendTaskCriterionResponse returns the recorded amendment and the run as it
// then stands — with its review re-opened.
type AmendTaskCriterionResponse struct {
	Amendment WorkflowTaskCriterionAmendmentView `json:"amendment"`
	Workflow  WorkflowRunDetailView              `json:"workflow"`
}

// WorkflowTaskCriterionAmendmentView is the read-only projection of one
// amendment: what the criterion was, what it became, who approved it and why.
type WorkflowTaskCriterionAmendmentView struct {
	ID                    string    `json:"id"`
	TaskID                string    `json:"taskId"`
	CriterionIndex        int64     `json:"criterionIndex"`
	OriginalCriterion     string    `json:"originalCriterion"`
	AmendedCriterion      string    `json:"amendedCriterion,omitempty"`
	Disposition           string    `json:"disposition" enum:"amended,declared_obsolete"`
	Reason                string    `json:"reason"`
	Evidence              []string  `json:"evidence"`
	ApprovedBy            string    `json:"approvedBy"`
	SupersededReviewRunID string    `json:"supersededReviewRunId,omitempty"`
	CreatedAt             time.Time `json:"createdAt"`
}

// amendTaskCriterion is the supported alternative to editing the database by
// hand or replanning an entire objective when a criterion has gone stale.
//
// It never approves the work. It records the amendment, applies it, and
// re-opens an independent review under criteria that describe reality.
func (c *WorkflowsController) amendTaskCriterion(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/workflows/{workflowId}/tasks/{taskId}/criteria/amend")
		return
	}
	var body AmendTaskCriterionRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "WORKFLOW_INVALID", "Malformed amendment body", nil)
		return
	}
	amendment, detail, err := c.Svc.AmendTaskCriterion(r.Context(), workflowsvc.TaskCriterionAmendment{
		RunID:             chi.URLParam(r, "workflowId"),
		TaskID:            chi.URLParam(r, "taskId"),
		CriterionIndex:    body.CriterionIndex,
		OriginalCriterion: body.OriginalCriterion,
		AmendedCriterion:  body.AmendedCriterion,
		Reason:            body.Reason,
		Evidence:          body.Evidence,
		ApprovedBy:        body.ApprovedBy,
	})
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, AmendTaskCriterionResponse{
		Amendment: WorkflowTaskCriterionAmendmentView{
			ID: amendment.ID, TaskID: amendment.TaskID, CriterionIndex: amendment.CriterionIndex,
			OriginalCriterion: amendment.OriginalCriterion, AmendedCriterion: amendment.AmendedCriterion,
			Disposition: string(amendment.Disposition), Reason: amendment.Reason,
			Evidence: amendment.Evidence, ApprovedBy: amendment.ApprovedBy,
			SupersededReviewRunID: amendment.SupersededReviewRunID, CreatedAt: amendment.CreatedAt,
		},
		Workflow: c.workflowRunDetailView(r.Context(), detail),
	})
}

// resumeAmendedTaskReview finishes an amendment whose fresh review never
// opened — a daemon that died between the amendment and the re-open, or a
// re-open that was itself wrong. It records no second amendment; it re-applies
// the consequences of the one already on file, idempotently.
func (c *WorkflowsController) resumeAmendedTaskReview(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/workflows/{workflowId}/tasks/{taskId}/criteria/resume-review")
		return
	}
	detail, err := c.Svc.ResumeAmendedTaskReview(r.Context(), chi.URLParam(r, "workflowId"), chi.URLParam(r, "taskId"))
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, WorkflowRunResponse{Workflow: c.workflowRunDetailView(r.Context(), detail)})
}

func writeWorkflowError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, workflowsvc.ErrInvalid):
		envelope.WriteAPIError(w, r, http.StatusUnprocessableEntity, "unprocessable", "WORKFLOW_INVALID", err.Error(), nil)
	case errors.Is(err, workflowsvc.ErrNotFound):
		envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "WORKFLOW_NOT_FOUND", err.Error(), nil)
	case errors.Is(err, workflowsvc.ErrAlreadyTerminal):
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict", "WORKFLOW_ALREADY_TERMINAL", err.Error(), nil)
	case errors.Is(err, workflowsvc.ErrPlanLocked):
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict", "WORKFLOW_PLAN_LOCKED", err.Error(), nil)
	default:
		envelope.WriteAPIError(w, r, http.StatusInternalServerError, "internal", "WORKFLOW_OPERATION_FAILED", "Workflow operation failed", nil)
	}
}
