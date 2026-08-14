package workflow_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

func validMasterPlan() workflowcore.MasterPlan {
	return workflowcore.MasterPlan{Version: "v1", Objective: "Build users", Summary: "two steps", Steps: []workflowcore.PlannedStep{
		{ID: "model", Title: "Model", Description: "Add the user model.", Dependencies: []string{}, AcceptanceCriteria: []string{"Model validates names."}, Verify: workflowcore.VerificationPlan{Commands: []workflowcore.VerificationCommandCheck{{Command: "go", Args: []string{"test", "./..."}, TimeoutSeconds: 60, RequiredExitCode: 0, RetrySafe: true}}, Files: []workflowcore.VerificationFileCheck{}}},
		{ID: "tests", Title: "Tests", Description: "Add behavior tests.", Dependencies: []string{"model"}, AcceptanceCriteria: []string{"Tests cover invalid users."}, Verify: workflowcore.VerificationPlan{Commands: []workflowcore.VerificationCommandCheck{{Command: "go", Args: []string{"test", "./..."}, TimeoutSeconds: 60, RequiredExitCode: 0, RetrySafe: true}}, Files: []workflowcore.VerificationFileCheck{}}},
	}}
}

func TestMasterPlanValidationRejectsUnknownDependencyCycleAndUnsafeCommand(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*workflowcore.MasterPlan)
		want   string
	}{
		{"unknown dependency", func(p *workflowcore.MasterPlan) { p.Steps[1].Dependencies = []string{"missing"} }, "unknown dependency"},
		{"cycle", func(p *workflowcore.MasterPlan) { p.Steps[0].Dependencies = []string{"tests"} }, "cycle"},
		{"unsafe command", func(p *workflowcore.MasterPlan) { p.Steps[0].Verify.Commands[0].Command = "bash" }, "not allowed"},
		{"escaping working directory", func(p *workflowcore.MasterPlan) { p.Steps[0].Verify.Commands[0].WorkingDirectory = "../outside" }, "inside the workspace"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validMasterPlan()
			tt.mutate(&p)
			_, v, _ := workflowcore.NormalizeAndValidatePlan(p, p.Objective, workflowcore.MaxPlanSteps)
			if v.Valid || !strings.Contains(strings.Join(v.Errors, " "), tt.want) {
				t.Fatalf("validation=%+v, want %q", v, tt.want)
			}
		})
	}
}

func TestMasterPlanHashIsDeterministicAndStructural(t *testing.T) {
	p := validMasterPlan()
	_, v, h1 := workflowcore.NormalizeAndValidatePlan(p, p.Objective, 12)
	if !v.Valid {
		t.Fatal(v.Errors)
	}
	_, _, h2 := workflowcore.NormalizeAndValidatePlan(p, p.Objective, 12)
	if h1 != h2 {
		t.Fatalf("hash changed: %s %s", h1, h2)
	}
	p.Steps[0].Title = "Changed"
	_, _, h3 := workflowcore.NormalizeAndValidatePlan(p, p.Objective, 12)
	if h3 == h1 {
		t.Fatal("structural change did not change hash")
	}
}

type staticPlanner struct {
	plan  workflowcore.MasterPlan
	calls int
}

func (p *staticPlanner) Generate(context.Context, workflowcore.PlannerRequest) (workflowcore.PlannerResponse, error) {
	p.calls++
	return workflowcore.PlannerResponse{Plan: p.plan, Provider: "fake", Model: "fake-v1"}, nil
}
func (p *staticPlanner) Descriptor() (string, string) { return "fake", "fake-v1" }

type staticContext struct{}

func (staticContext) Build(_ context.Context, p domain.ProjectRecord) (workflowcore.PlannerContext, error) {
	return workflowcore.PlannerContext{Version: "v1", ProjectID: string(p.ID), ProjectPath: p.Path, Documents: []workflowcore.PlannerDocument{}}, nil
}

func TestInvalidGeneratedPlanCreatesNoTasksOrWorkers(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	project := domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: time.Now().UTC()}
	if err := store.UpsertProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	p := validMasterPlan()
	p.Steps[1].Dependencies = []string{"missing"}
	planner := &staticPlanner{plan: p}
	c := workflowcore.New(workflowcore.Deps{Store: store, Projects: store, Planner: planner, PlannerContextBuilder: staticContext{}})
	created, err := c.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := c.GeneratePlan(ctx, created.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Run.State != domain.WorkflowRunNeedsAttention || detail.Plan == nil || detail.Plan.Status != domain.WorkflowPlanInvalid {
		t.Fatalf("detail=%+v", detail)
	}
	if len(detail.Tasks) != 0 {
		t.Fatalf("invalid plan created %d tasks", len(detail.Tasks))
	}
	if planner.calls != 1 {
		t.Fatalf("planner calls=%d", planner.calls)
	}
}

func newMasterFixture(t *testing.T, plan workflowcore.MasterPlan, mode domain.WorkflowPlanApprovalMode) (*workflowcore.Coordinator, *staticPlanner, string) {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	project := domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: time.Now().UTC()}
	if err := store.UpsertProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	planner := &staticPlanner{plan: plan}
	c := workflowcore.New(workflowcore.Deps{Store: store, Projects: store, Planner: planner, PlannerContextBuilder: staticContext{}})
	created, err := c.CreateObjectiveRun(ctx, "p", "Build users", mode)
	if err != nil {
		t.Fatal(err)
	}
	return c, planner, created.Run.ID
}

func TestGenerateAndApproveAreIdempotent(t *testing.T) {
	c, planner, runID := newMasterFixture(t, validMasterPlan(), domain.WorkflowPlanApprovalManual)
	ctx := context.Background()
	first, err := c.GeneratePlan(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.GeneratePlan(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if planner.calls != 1 || first.Plan.PlanHash != second.Plan.PlanHash || len(second.Tasks) != 2 {
		t.Fatalf("calls=%d first=%+v second=%+v", planner.calls, first.Plan, second.Plan)
	}
	approved, err := c.ApprovePlan(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	again, err := c.ApprovePlan(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if approved.Plan.Status != domain.WorkflowPlanApproved || len(again.Tasks) != 2 || again.Tasks[0].ExecutionRunID == nil {
		t.Fatalf("approved=%+v again=%+v", approved, again)
	}
	if *approved.Tasks[0].ExecutionRunID != *again.Tasks[0].ExecutionRunID {
		t.Fatal("approval duplicated execution run")
	}
}

func TestAutoApprovalDispatchesFirstEligibleTask(t *testing.T) {
	c, _, runID := newMasterFixture(t, validMasterPlan(), domain.WorkflowPlanApprovalAuto)
	detail, err := c.GeneratePlan(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Plan.Status != domain.WorkflowPlanApproved || detail.Run.State != domain.WorkflowRunRunning || detail.Tasks[0].State != domain.WorkflowTaskRunning || detail.Tasks[0].ExecutionRunID == nil {
		t.Fatalf("detail=%+v", detail)
	}
}

func TestPlannerRecoveryRunningIsAmbiguousButRespondedFinalizes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		responded bool
		want      domain.WorkflowPlanStatus
		tasks     int
	}{{"running", false, domain.WorkflowPlanInvalid, 0}, {"responded", true, domain.WorkflowPlanValidated, 2}} {
		t.Run(tc.name, func(t *testing.T) {
			store := sqlitetest.MustOpen(t)
			ctx := context.Background()
			project := domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: time.Now().UTC()}
			if err := store.UpsertProject(ctx, project); err != nil {
				t.Fatal(err)
			}
			c := workflowcore.New(workflowcore.Deps{Store: store, Projects: store, Planner: &staticPlanner{plan: validMasterPlan()}, PlannerContextBuilder: staticContext{}})
			created, err := c.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			if ok, err := store.StartWorkflowPlanCommand(ctx, created.Run.ID, "fake", "fake-v1", "{}", now); err != nil || !ok {
				t.Fatalf("start=%v %v", ok, err)
			}
			steps, _ := store.ListWorkflowSteps(ctx, created.Run.ID)
			_, _ = store.UpdateWorkflowStepState(ctx, steps[0].ID, domain.WorkflowStepReady, domain.WorkflowStepRunning, now)
			if tc.responded {
				raw, _ := json.Marshal(validMasterPlan())
				if ok, err := store.PersistWorkflowPlanResponse(ctx, created.Run.ID, string(raw), now); err != nil || !ok {
					t.Fatalf("respond=%v %v", ok, err)
				}
			}
			if err := c.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			detail, err := c.GetRun(ctx, created.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if detail.Plan.Status != tc.want || len(detail.Tasks) != tc.tasks {
				t.Fatalf("plan=%s tasks=%d", detail.Plan.Status, len(detail.Tasks))
			}
		})
	}
}
