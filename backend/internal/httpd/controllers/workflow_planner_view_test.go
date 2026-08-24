package controllers_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// plannerRunDetail is one master run whose two tasks between them exercise
// every field of the planner projection: a completed task that landed on the
// target through an AO worktree, and a blocked one held by a write conflict.
func plannerRunDetail(now time.Time) workflowcore.RunDetail {
	return workflowcore.RunDetail{
		Run:  domain.WorkflowRun{ID: "wf-1", ProjectID: "proj-1", Objective: "ship it", State: domain.WorkflowRunRunning, CreatedAt: now, UpdatedAt: now},
		Plan: &domain.WorkflowPlanRecord{WorkflowRunID: "wf-1", Status: domain.WorkflowPlanApproved},
		Tasks: []domain.WorkflowTask{
			{ID: "task-a", PlanStepID: "s1", WorkflowRunID: "wf-1", Ordinal: 1, Title: "Backend API", State: domain.WorkflowTaskCompleted, ScopeJSON: "{}"},
			{ID: "task-b", PlanStepID: "s2", WorkflowRunID: "wf-1", Ordinal: 2, Title: "Board UI", State: domain.WorkflowTaskBlocked, Dependencies: []string{"task-a"}, ScopeJSON: "{}"},
		},
		TaskPlanner: []workflowcore.TaskPlannerView{
			{
				TaskID: "task-a", Ordinal: 1, Status: workflowcore.TaskPlannerIntegrated,
				ExecutionStrategy: domain.WorkflowTaskExecutionParallel,
				ExecutionMode:     domain.ExecutionSmartParallelWorktrees,
				ParallelGroup:     1, ParallelGroupSize: 1,
				WriteScope: workflowcore.TaskWriteScopeView{
					Source: domain.WorkflowTaskScopeObserved, WritePaths: []string{"backend/internal/httpd"},
					Packages: []string{"backend/internal/httpd"},
				},
				Worktree: &workflowcore.TaskWorktreeView{
					Path: "/data/worktrees/a", Branch: "ao/wf-1-task-a", TargetBranch: "main",
					BaseSHA: "beef", State: domain.TaskWorktreeReleased, IntegratedSHA: "cafe", BranchDeleted: true,
				},
				Integration: &workflowcore.TaskIntegrationView{
					Outcome: "integrated", Strategy: "rebase_fast_forward",
					SourceSHA: "aaa", TargetBeforeSHA: "bbb", TargetAfterSHA: "cafe", BaseSHA: "beef", Replayed: true,
				},
			},
			{
				TaskID: "task-b", Ordinal: 2, Status: workflowcore.TaskPlannerWaitingForConflict,
				ExecutionStrategy: domain.WorkflowTaskExecutionSerialized,
				ExecutionMode:     domain.ExecutionIsolatedWorktree,
				Downgrade: &domain.WorkflowTaskExecutionDowngrade{
					From: domain.ExecutionSmartParallelWorktrees, To: domain.ExecutionIsolatedWorktree,
					Serial: true, Reason: "probable_write_conflict", Conflicts: []string{"task-a"},
				},
				Dependencies: []string{"task-a"}, IntegrationDependencies: []string{"task-a"},
				WaitingReason: domain.WorkflowTaskWaitingConflict,
				ParallelGroup: 2, ParallelGroupSize: 1,
				WriteScope: workflowcore.TaskWriteScopeView{
					Source: domain.WorkflowTaskScopeEstimated, WritePaths: []string{"frontend/src/renderer"},
				},
			},
		},
	}
}

type plannerTaskResponse struct {
	Workflow struct {
		Tasks []struct {
			ID      string `json:"id"`
			Planner *struct {
				Status                  string   `json:"status"`
				ExecutionStrategy       string   `json:"executionStrategy"`
				ExecutionMode           string   `json:"executionMode"`
				Dependencies            []string `json:"dependencies"`
				IntegrationDependencies []string `json:"integrationDependencies"`
				WaitingReason           string   `json:"waitingReason"`
				ParallelGroup           int      `json:"parallelGroup"`
				ParallelGroupSize       int      `json:"parallelGroupSize"`
				Downgrade               *struct {
					From      string   `json:"from"`
					To        string   `json:"to"`
					Serial    bool     `json:"serial"`
					Conflicts []string `json:"conflicts"`
				} `json:"downgrade"`
				WriteScope struct {
					Source     string   `json:"source"`
					WritePaths []string `json:"writePaths"`
					Packages   []string `json:"packages"`
					ReadPaths  []string `json:"readPaths"`
				} `json:"writeScope"`
				Worktree *struct {
					Path          string `json:"path"`
					Branch        string `json:"branch"`
					TargetBranch  string `json:"targetBranch"`
					BaseSHA       string `json:"baseSha"`
					State         string `json:"state"`
					IntegratedSHA string `json:"integratedSha"`
				} `json:"worktree"`
				Integration *struct {
					Outcome         string `json:"outcome"`
					Strategy        string `json:"strategy"`
					TargetBeforeSHA string `json:"targetBeforeSha"`
					TargetAfterSHA  string `json:"targetAfterSha"`
					Replayed        bool   `json:"replayed"`
				} `json:"integration"`
			} `json:"planner"`
		} `json:"tasks"`
	} `json:"workflow"`
}

// Everything the task API is required to expose, on one request: strategy,
// dependencies, waiting reason, parallel group, probable write scope,
// worktree/branch and integration state.
func TestWorkflowGetRunSurfacesTaskPlannerFields(t *testing.T) {
	svc := &fakeWorkflowService{detail: plannerRunDetail(time.Now().UTC())}
	srv := newWorkflowTestServer(t, svc)

	body, status, headers := doRequest(t, srv, "GET", "/api/v1/workflows/wf-1", "")
	assertJSON(t, headers)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var got plannerTaskResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	if len(got.Workflow.Tasks) != 2 {
		t.Fatalf("tasks = %d, want 2: %s", len(got.Workflow.Tasks), body)
	}

	a := got.Workflow.Tasks[0]
	if a.Planner == nil {
		t.Fatal("task a has no planner section")
	}
	if a.Planner.Status != "integrated" {
		t.Errorf("status = %q", a.Planner.Status)
	}
	if a.Planner.ExecutionStrategy != "parallel" || a.Planner.ExecutionMode != "smart_parallel_worktrees" {
		t.Errorf("strategy/mode = %q/%q", a.Planner.ExecutionStrategy, a.Planner.ExecutionMode)
	}
	if a.Planner.WriteScope.Source != "observed" || len(a.Planner.WriteScope.WritePaths) != 1 {
		t.Errorf("write scope = %+v", a.Planner.WriteScope)
	}
	// An empty list stays a list: a client that renders "writes nothing" from
	// [] and "unknown" from a missing field must be able to tell them apart.
	if a.Planner.WriteScope.ReadPaths == nil {
		t.Error("readPaths is null, want an empty array")
	}
	if a.Planner.Worktree == nil {
		t.Fatal("task a has no worktree section")
	}
	if a.Planner.Worktree.Branch != "ao/wf-1-task-a" || a.Planner.Worktree.TargetBranch != "main" {
		t.Errorf("worktree = %+v", *a.Planner.Worktree)
	}
	if a.Planner.Worktree.State != "released" || a.Planner.Worktree.IntegratedSHA != "cafe" {
		t.Errorf("worktree lifecycle = %+v", *a.Planner.Worktree)
	}
	if a.Planner.Integration == nil {
		t.Fatal("task a has no integration section")
	}
	if a.Planner.Integration.Strategy != "rebase_fast_forward" || a.Planner.Integration.TargetAfterSHA != "cafe" || !a.Planner.Integration.Replayed {
		t.Errorf("integration = %+v", *a.Planner.Integration)
	}

	b := got.Workflow.Tasks[1]
	if b.Planner == nil {
		t.Fatal("task b has no planner section")
	}
	if b.Planner.Status != "waiting_for_conflict" || b.Planner.WaitingReason != "waiting_for_write_conflict" {
		t.Errorf("status/waitingReason = %q/%q", b.Planner.Status, b.Planner.WaitingReason)
	}
	if b.Planner.ParallelGroup != 2 || b.Planner.ParallelGroupSize != 1 {
		t.Errorf("parallel group = %d/%d", b.Planner.ParallelGroup, b.Planner.ParallelGroupSize)
	}
	// Dependencies reach the wire as plan-step ids, exactly like the task's own
	// `dependencies` field — never as the internal task id.
	if len(b.Planner.Dependencies) != 1 || b.Planner.Dependencies[0] != "s1" {
		t.Errorf("dependencies = %v, want the plan-step id", b.Planner.Dependencies)
	}
	if len(b.Planner.IntegrationDependencies) != 1 || b.Planner.IntegrationDependencies[0] != "s1" {
		t.Errorf("integrationDependencies = %v", b.Planner.IntegrationDependencies)
	}
	if b.Planner.Downgrade == nil || !b.Planner.Downgrade.Serial {
		t.Fatalf("downgrade = %+v", b.Planner.Downgrade)
	}
	if len(b.Planner.Downgrade.Conflicts) != 1 || b.Planner.Downgrade.Conflicts[0] != "s1" {
		t.Errorf("downgrade conflicts = %v, want plan-step ids", b.Planner.Downgrade.Conflicts)
	}
	// A task with no AO worktree reports none rather than an empty object.
	if b.Planner.Worktree != nil {
		t.Errorf("task b got a worktree section: %+v", *b.Planner.Worktree)
	}
	if b.Planner.Integration != nil {
		t.Errorf("task b got an integration section: %+v", *b.Planner.Integration)
	}
}

// A run whose daemon produced no projection at all must still serve its tasks.
// The planner section is additive; its absence is not an error shape.
func TestWorkflowGetRunOmitsPlannerWhenNotProjected(t *testing.T) {
	detail := plannerRunDetail(time.Now().UTC())
	detail.TaskPlanner = nil
	svc := &fakeWorkflowService{detail: detail}
	srv := newWorkflowTestServer(t, svc)

	body, status, _ := doRequest(t, srv, "GET", "/api/v1/workflows/wf-1", "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var got plannerTaskResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	if len(got.Workflow.Tasks) != 2 {
		t.Fatalf("tasks = %d, want 2", len(got.Workflow.Tasks))
	}
	for _, task := range got.Workflow.Tasks {
		if task.Planner != nil {
			t.Errorf("task %s got a planner section with nothing projected", task.ID)
		}
	}
}
