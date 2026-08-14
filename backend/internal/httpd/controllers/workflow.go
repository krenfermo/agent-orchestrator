package controllers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	workflowsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/workflow"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

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
}

// WorkflowRunDetailView is a workflow run plus its steps and their attempts.
type WorkflowRunDetailView struct {
	Run   WorkflowRunView    `json:"run"`
	Steps []WorkflowStepView `json:"steps"`
	Plan  *WorkflowPlanView  `json:"plan,omitempty"`
	Tasks []WorkflowTaskView `json:"tasks,omitempty"`
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
		ID:          run.ID,
		ProjectID:   run.ProjectID,
		Objective:   run.Objective,
		State:       run.State,
		CreatedAt:   run.CreatedAt,
		UpdatedAt:   run.UpdatedAt,
		CompletedAt: run.CompletedAt,
		CancelledAt: run.CancelledAt,
		NextAction:  nextAction,
	}
}

func workflowRunDetailView(detail workflowcore.RunDetail) WorkflowRunDetailView {
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
		})
	}
	view := WorkflowRunDetailView{Run: workflowRunView(detail.Run, detail.NextAction), Steps: steps}
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
	return view
}

// WorkflowsController owns the /workflows routes. A nil Svc returns 501.
type WorkflowsController struct {
	Svc workflowsvc.Manager
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
	envelope.WriteJSON(w, http.StatusCreated, WorkflowRunResponse{Workflow: workflowRunDetailView(detail)})
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
	envelope.WriteJSON(w, http.StatusOK, WorkflowRunResponse{Workflow: workflowRunDetailView(detail)})
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
	envelope.WriteJSON(w, http.StatusOK, WorkflowRunResponse{Workflow: workflowRunDetailView(detail)})
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
	envelope.WriteJSON(w, http.StatusOK, WorkflowRunResponse{Workflow: workflowRunDetailView(detail)})
}

func (c *WorkflowsController) get(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/workflows/{workflowId}")
		return
	}
	detail, err := c.Svc.GetRun(r.Context(), chi.URLParam(r, "workflowId"))
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, WorkflowRunResponse{Workflow: workflowRunDetailView(detail)})
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
	envelope.WriteJSON(w, http.StatusOK, WorkflowRunResponse{Workflow: workflowRunDetailView(detail)})
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
	envelope.WriteJSON(w, http.StatusOK, WorkflowRunResponse{Workflow: workflowRunDetailView(detail)})
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
	envelope.WriteJSON(w, http.StatusOK, WorkflowRunResponse{Workflow: workflowRunDetailView(detail)})
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
