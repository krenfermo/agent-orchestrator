package controllers

import (
	"context"
	"encoding/json"
	"errors"
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
	ErrorClass    domain.WorkflowErrorClass     `json:"errorClass,omitempty" enum:"rate_limited,auth,transient,tool,test_failed,review_changes_requested,session_create_failed,agent_start_failed,prompt_delivery_failed,runtime_failed,worker_terminated_unexpectedly,ambiguous_worker_state,reviewer_launch_failed,fix_budget_exhausted,verify_command_failed,verify_timeout,verify_environment_error,verify_artifact_missing,verify_artifact_mismatch,verify_workspace_changed,verify_ambiguous,capacity_exhausted,binary_missing"`
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
		})
	}
	runView := workflowRunView(detail.Run, detail.NextAction)
	runView.NextWakeAt = detail.NextWakeAt
	runView.WaitReason = detail.WaitReason
	runView.WakeAttemptCount = detail.WakeAttemptCount
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
		view.Tasks = append(view.Tasks, WorkflowTaskView{ID: task.PlanStepID, Number: task.Ordinal, Title: task.Title, Description: task.Description, Dependencies: deps, AcceptanceCriteria: criteria, Verify: verify, State: task.State, ExecutionWorkflowID: execID})
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

func (c *WorkflowsController) stampOwner(r *http.Request, id string) {
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
	// run on its default policy_snapshot exactly as before.
	if applier, ok := c.Svc.(workflowsvc.ExecutionPolicyApplier); ok {
		_ = applier.ApplyExecutionPolicySnapshot(r.Context(), id, user.ID)
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
	r.Get("/workflows/{workflowId}", c.get)
	r.Get("/workflows", c.list)
	r.Post("/workflows/{workflowId}/cancel", c.cancel)
	r.Post("/workflows/{workflowId}/start", c.start)
	r.Post("/workflows/{workflowId}/continue", c.continueRun)
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
	c.stampOwner(r, detail.Run.ID)
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
