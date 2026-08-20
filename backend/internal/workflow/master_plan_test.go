package workflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
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

// failingPlanner is a workflow.Planner test double that always returns a
// fixed error, letting Checkpoint 8P-E.10's coordinator-level classification
// tests below control exactly which sentinel (if any) the planner adapter
// would have wrapped, without spinning up a real subprocess.
type failingPlanner struct{ err error }

func (p failingPlanner) Generate(context.Context, workflowcore.PlannerRequest) (workflowcore.PlannerResponse, error) {
	return workflowcore.PlannerResponse{}, p.err
}
func (p failingPlanner) Descriptor() (string, string) { return "fake", "fake-v1" }

// medusaSizedObjective mirrors the size of the real MEDUSA workflow prompts
// that motivated Checkpoint 8P-E.10 -- tens of KB of detailed requirements
// text -- so the classification regression tests below exercise the same
// payload shape that produced the original planner_timeout /
// planner_parse_failed incidents, not a toy one-line objective.
func medusaSizedObjective() string {
	return strings.Repeat("CHECKPOINT 8P-E.10 PLANNER ROBUSTNESS FOR LONG AUTONOMOUS WORKFLOWS: decompose this large, detailed objective into small independently verifiable units with concrete acceptance criteria and safe structured verification checks. ", 400)
}

func newFailingMasterFixture(t *testing.T, plannerErr error) (*workflowcore.Coordinator, string) {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	project := domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: time.Now().UTC()}
	if err := store.UpsertProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	c := workflowcore.New(workflowcore.Deps{Store: store, Projects: store, Planner: failingPlanner{err: plannerErr}, PlannerContextBuilder: staticContext{}})
	created, err := c.CreateObjectiveRun(ctx, "p", medusaSizedObjective(), domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatal(err)
	}
	return c, created.Run.ID
}

// TestGeneratePlan_LongPromptPlannerTimeout_ClassifiesAsPlannerTimeout is the
// regression test for the real MEDUSA workflow evidence that motivated
// Checkpoint 8P-E.10: a planner call for a long, detailed objective timing
// out with error_class=planner_timeout, validation_json {"valid":false,...}.
func TestGeneratePlan_LongPromptPlannerTimeout_ClassifiesAsPlannerTimeout(t *testing.T) {
	c, runID := newFailingMasterFixture(t, fmt.Errorf("planner timeout: %w: context deadline exceeded", ports.ErrPlannerTimeout))
	detail, err := c.GeneratePlan(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Plan.Status != domain.WorkflowPlanInvalid || detail.Plan.ErrorClass != "planner_timeout" {
		t.Fatalf("plan=%+v", detail.Plan)
	}
	if detail.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state=%s", detail.Run.State)
	}
}

// TestGeneratePlan_LongPromptPlannerParseFailure_ClassifiesAsPlannerParseFailed
// is the regression test for the second real MEDUSA incident: the planner
// subprocess exiting successfully but its stdout not parsing into a plan
// envelope (validation error "invalid character 'I' looking for beginning of
// value"), classified as error_class=planner_parse_failed.
func TestGeneratePlan_LongPromptPlannerParseFailure_ClassifiesAsPlannerParseFailed(t *testing.T) {
	c, runID := newFailingMasterFixture(t, fmt.Errorf("planner parse envelope: %w: invalid character 'I' looking for beginning of value", ports.ErrPlannerOutputMalformed))
	detail, err := c.GeneratePlan(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Plan.Status != domain.WorkflowPlanInvalid || detail.Plan.ErrorClass != "planner_parse_failed" {
		t.Fatalf("plan=%+v", detail.Plan)
	}
	if detail.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state=%s", detail.Run.State)
	}
}

// TestGeneratePlan_ClassificationIsSentinelBased_NotObjectiveTextSubstring is
// the regression test for the classification bug the sentinel-based rewrite
// fixed: before Checkpoint 8P-E.10, GeneratePlan classified planner failures
// by substring-matching err.Error() for "timeout"/"parse", so an unrelated
// planner failure could be misclassified purely because the workflow's own
// (long, detailed) objective text happened to contain those words. This
// objective's medusaSizedObjective() text does not contain "timeout" or
// "parse", but a truly generic failure must still land on the conservative
// default class regardless of objective content.
func TestGeneratePlan_ClassificationIsSentinelBased_NotObjectiveTextSubstring(t *testing.T) {
	c, runID := newFailingMasterFixture(t, errors.New("planner exited with an unrecognized internal error"))
	detail, err := c.GeneratePlan(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Plan.ErrorClass != "planner_start_failed" {
		t.Fatalf("generic unclassified failure must default to planner_start_failed, got %q", detail.Plan.ErrorClass)
	}
}
