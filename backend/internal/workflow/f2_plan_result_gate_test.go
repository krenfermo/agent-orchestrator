package workflow_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// f2_plan_result_gate_test.go — the coordinator half of F2's test matrix.
//
// Before the fix, the placeholder below reached workflow_tasks and a dispatched
// worker: it is schema valid, so validation passed it, and approvalMode=auto
// approved it.

// f2GatePlaceholderPlan is byte-for-byte the shape AO persisted in the
// incident, right down to the `npm test` verification.
func f2GatePlaceholderPlan() workflowcore.MasterPlan {
	return workflowcore.MasterPlan{
		Version: "v1", Objective: "Extend the greetings module", Summary: "test",
		Steps: []workflowcore.PlannedStep{{
			ID: "s1", Title: "t", Description: "d",
			Dependencies: []string{}, AcceptanceCriteria: []string{"a"},
			Verify: workflowcore.VerificationPlan{
				Commands: []workflowcore.VerificationCommandCheck{{Command: "npm", Args: []string{"test"}, TimeoutSeconds: 60, RetrySafe: true}},
				Files:    []workflowcore.VerificationFileCheck{},
			},
		}},
	}
}

// f2GateRealPlan is a legitimate one-step plan: proof the gate refuses
// placeholders, not brevity.
func f2GateRealPlan() workflowcore.MasterPlan {
	return workflowcore.MasterPlan{
		Version: "v1", Objective: "Extend the greetings module", Summary: "Add the farewell entry point and document it.",
		Steps: []workflowcore.PlannedStep{{
			ID:                 "s1",
			Title:              "Add the farewell entry point",
			Description:        "Add src/farewell.js exporting farewell(name), export it from src/index.js, and document it in the README.",
			Dependencies:       []string{},
			AcceptanceCriteria: []string{"src/index.js exports both greet and farewell and npm run verify passes."},
			Verify: workflowcore.VerificationPlan{
				Commands: []workflowcore.VerificationCommandCheck{{Command: "npm", Args: []string{"test"}, TimeoutSeconds: 60, RetrySafe: true}},
				Files:    []workflowcore.VerificationFileCheck{},
			},
		}},
	}
}

func f2GateCoordinator(t *testing.T, planner workflowcore.Planner) (*workflowcore.Coordinator, *sqlite.Store, string) {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	c := workflowcore.New(workflowcore.Deps{
		Store: store, Projects: store, Planner: planner, PlannerContextBuilder: staticContext{},
	})
	created, err := c.CreateObjectiveRun(ctx, "p",
		"Extend the greetings module with a farewell entry point, a shared normalization helper and documentation.",
		domain.WorkflowPlanApprovalAuto)
	if err != nil {
		t.Fatalf("CreateObjectiveRun: %v", err)
	}
	return c, store, created.Run.ID
}

// C + J: a schema-valid placeholder is refused, and Approval=Automatic does not
// weaken that. No task, no child run, no dispatched worker.
func TestF2_PlaceholderPlanIsNotApprovedOrDispatched(t *testing.T) {
	c, store, runID := f2GateCoordinator(t, &staticPlanner{plan: f2GatePlaceholderPlan()})
	ctx := context.Background()

	detail, err := c.GeneratePlan(ctx, runID)
	if err != nil {
		t.Fatalf("GeneratePlan: %v", err)
	}
	if detail.Plan.Status == domain.WorkflowPlanApproved {
		t.Fatal("a placeholder plan was approved; Automatic approval must never weaken plan validity")
	}
	tasks, err := store.ListWorkflowTasks(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Fatalf("a placeholder plan produced %d durable tasks; want none", len(tasks))
	}
	runs, err := store.ListWorkflowRuns(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("a placeholder plan created %d runs; only the parent should exist", len(runs))
	}
}

// B, at the coordinator: a legitimately concise one-step plan still runs.
func TestF2_ConciseRealPlanStillApproved(t *testing.T) {
	c, store, runID := f2GateCoordinator(t, &staticPlanner{plan: f2GateRealPlan()})
	ctx := context.Background()

	detail, err := c.GeneratePlan(ctx, runID)
	if err != nil {
		t.Fatalf("GeneratePlan: %v", err)
	}
	if detail.Plan.Status != domain.WorkflowPlanApproved {
		t.Fatalf("plan status = %s, want approved for a real one-step plan", detail.Plan.Status)
	}
	tasks, err := store.ListWorkflowTasks(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
}

// G, at the coordinator: the run retries and the SECOND, real plan is the one
// that becomes durable — persisted once, with one task, not two.
func TestF2_RetryPersistsTheRecoveredPlanExactlyOnce(t *testing.T) {
	planner := &swappablePlanner{plan: f2GatePlaceholderPlan()}
	c, store, runID := f2GateCoordinator(t, planner)
	ctx := context.Background()

	if _, err := c.GeneratePlan(ctx, runID); err != nil {
		t.Fatalf("first GeneratePlan: %v", err)
	}
	planner.plan = f2GateRealPlan()
	detail, err := c.GeneratePlan(ctx, runID)
	if err != nil {
		t.Fatalf("second GeneratePlan: %v", err)
	}
	if detail.Plan.Status != domain.WorkflowPlanApproved {
		t.Fatalf("plan status = %s, want approved after the retry recovered a real plan", detail.Plan.Status)
	}
	tasks, err := store.ListWorkflowTasks(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want exactly 1 (the recovered plan, persisted once)", len(tasks))
	}
	if tasks[0].Title == "t" {
		t.Fatal("the placeholder task survived the retry")
	}
}

// H: every retry inconsistent ends in needs_attention naming the specific
// class — never planner_ambiguous, which would tell a person to answer a
// question that was never asked.
func TestF2_ExhaustedRetriesParkWithTheSpecificClass(t *testing.T) {
	c, store, runID := f2GateCoordinator(t, &staticPlanner{plan: f2GatePlaceholderPlan()})
	ctx := context.Background()

	for i := 0; i < 6; i++ {
		if _, err := c.GeneratePlan(ctx, runID); err != nil {
			t.Fatalf("GeneratePlan #%d: %v", i+1, err)
		}
		run, ok, err := store.GetWorkflowRun(ctx, runID)
		if err != nil || !ok {
			t.Fatal(err)
		}
		if run.State == domain.WorkflowRunNeedsAttention {
			break
		}
	}
	run, _, err := store.GetWorkflowRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %s, want needs_attention once the planner retries are exhausted", run.State)
	}
	plan, ok, err := store.GetWorkflowPlan(ctx, runID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if !strings.Contains(plan.ErrorClass, workflowcore.ReasonPlannerResultInconsistent) &&
		!strings.Contains(plan.ErrorClass, "exhausted") {
		t.Errorf("plan error class = %q, want it to name the result-inconsistency class", plan.ErrorClass)
	}
	if strings.Contains(plan.ErrorClass, workflowcore.ReasonPlannerAmbiguous) {
		t.Errorf("a lost result must never be reported as objective ambiguity, got %q", plan.ErrorClass)
	}
	tasks, _ := store.ListWorkflowTasks(ctx, runID)
	if len(tasks) != 0 {
		t.Fatalf("tasks = %d, want none: no placeholder may become executable work", len(tasks))
	}
}

// I: a restart between the provider's result and its persistence re-enters
// finalizeGeneratedPlan for the SAME attempt. It must reach the same verdict
// rather than being accepted because the daemon happened to come back.
func TestF2_RestartBeforePersistenceConvergesToTheSameVerdict(t *testing.T) {
	c, store, runID := f2GateCoordinator(t, &staticPlanner{plan: f2GatePlaceholderPlan()})
	ctx := context.Background()

	if _, err := c.GeneratePlan(ctx, runID); err != nil {
		t.Fatalf("GeneratePlan: %v", err)
	}
	// "Restart": a fresh coordinator over the same durable state.
	c2 := workflowcore.New(workflowcore.Deps{
		Store: store, Projects: store, Planner: &staticPlanner{plan: f2GatePlaceholderPlan()},
		PlannerContextBuilder: staticContext{},
	})
	detail, err := c2.GeneratePlan(ctx, runID)
	if err != nil {
		t.Fatalf("GeneratePlan after restart: %v", err)
	}
	if detail.Plan.Status == domain.WorkflowPlanApproved {
		t.Fatal("a restart approved a plan the pre-restart coordinator refused")
	}
	tasks, _ := store.ListWorkflowTasks(ctx, runID)
	if len(tasks) != 0 {
		t.Fatalf("tasks = %d, want none after restart", len(tasks))
	}
}

// K: the manual approval path cannot bless a refused plan either. A plan the
// gate rejected never reaches `validated`, which is the only state ApprovePlan
// promotes from.
func TestF2_ManualApprovalCannotBlessARefusedPlan(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	c := workflowcore.New(workflowcore.Deps{
		Store: store, Projects: store, Planner: &staticPlanner{plan: f2GatePlaceholderPlan()},
		PlannerContextBuilder: staticContext{},
	})
	created, err := c.CreateObjectiveRun(ctx, "p",
		"Extend the greetings module with a farewell entry point, a shared normalization helper and documentation.",
		domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GeneratePlan(ctx, created.Run.ID); err != nil {
		t.Fatalf("GeneratePlan: %v", err)
	}
	if _, err := c.ApprovePlan(ctx, created.Run.ID); err == nil {
		tasks, _ := store.ListWorkflowTasks(ctx, created.Run.ID)
		if len(tasks) != 0 {
			t.Fatalf("manual approval turned a refused placeholder into %d tasks", len(tasks))
		}
	}
	tasks, _ := store.ListWorkflowTasks(ctx, created.Run.ID)
	if len(tasks) != 0 {
		t.Fatalf("tasks = %d, want none", len(tasks))
	}
}

// The semantic floor's own boundaries, stated directly: what it rejects and
// what it must never reject.
func TestF2_PlanResultPlausibilityBoundaries(t *testing.T) {
	objective := "Extend the greetings module with a farewell entry point, a shared normalization helper and documentation."

	if reason := workflowcore.PlanResultPlausibility(f2GatePlaceholderPlan(), objective); reason == "" {
		t.Error("the incident's placeholder must be refused")
	}
	if reason := workflowcore.PlanResultPlausibility(f2GateRealPlan(), objective); reason != "" {
		t.Errorf("a real one-step plan must be accepted, got %q", reason)
	}

	// Non-Latin content of the same substance must behave identically: the
	// floors count runes, never words or bytes.
	spanish := f2GateRealPlan()
	spanish.Summary = "Añade el punto de entrada farewell y su documentación."
	spanish.Steps[0].Title = "Añadir el punto de entrada farewell"
	spanish.Steps[0].Description = "Crea src/farewell.js que exporte farewell(name), expórtalo desde src/index.js y documéntalo."
	spanish.Steps[0].AcceptanceCriteria = []string{"src/index.js exporta greet y farewell, y npm run verify pasa."}
	if reason := workflowcore.PlanResultPlausibility(spanish, objective); reason != "" {
		t.Errorf("a real plan in another language must be accepted, got %q", reason)
	}

	// Each placeholder field is caught on its own.
	for _, tt := range []struct {
		name   string
		mutate func(*workflowcore.MasterPlan)
		want   string
	}{
		{"summary", func(p *workflowcore.MasterPlan) { p.Summary = "test" }, "summary"},
		{"title", func(p *workflowcore.MasterPlan) { p.Steps[0].Title = "t" }, "title"},
		{"description", func(p *workflowcore.MasterPlan) { p.Steps[0].Description = "d" }, "description"},
		{"criterion", func(p *workflowcore.MasterPlan) { p.Steps[0].AcceptanceCriteria = []string{"a"} }, "criterion"},
		{"no criteria", func(p *workflowcore.MasterPlan) { p.Steps[0].AcceptanceCriteria = nil }, "acceptance criteria"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := f2GateRealPlan()
			tt.mutate(&p)
			reason := workflowcore.PlanResultPlausibility(p, objective)
			if reason == "" || !strings.Contains(reason, tt.want) {
				t.Errorf("reason = %q, want it to name the %s", reason, tt.want)
			}
		})
	}
}

// swappablePlanner returns whatever plan it currently holds, so a test can
// change the provider's answer between attempts.
type swappablePlanner struct {
	plan  workflowcore.MasterPlan
	calls int
}

func (p *swappablePlanner) Generate(context.Context, workflowcore.PlannerRequest) (workflowcore.PlannerResponse, error) {
	p.calls++
	return workflowcore.PlannerResponse{Plan: p.plan, Provider: "fake", Model: "fake-v1"}, nil
}
func (p *swappablePlanner) Descriptor() (string, string) { return "fake", "fake-v1" }
