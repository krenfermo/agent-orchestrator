package workflow_test

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// conflictingMasterPlan has two independent steps that both declare the same
// file, plus a third that depends on one of them.
func conflictingMasterPlan() workflowcore.MasterPlan {
	verify := workflowcore.VerificationPlan{
		Commands: []workflowcore.VerificationCommandCheck{{Command: "go", Args: []string{"test", "./..."}, WorkingDirectory: "backend", TimeoutSeconds: 60, RequiredExitCode: 0, RetrySafe: true}},
		Files:    []workflowcore.VerificationFileCheck{},
	}
	return workflowcore.MasterPlan{Version: "v1", Objective: "Build users", Summary: "three steps", Steps: []workflowcore.PlannedStep{
		{
			ID: "model", Title: "Model", Description: "Add the user model.", Dependencies: []string{},
			AcceptanceCriteria: []string{"Model validates names."}, Verify: verify,
			Files: []string{"backend/internal/domain/user.go"},
		},
		{
			ID: "roles", Title: "Roles", Description: "Add the role enum.", Dependencies: []string{},
			AcceptanceCriteria: []string{"Roles are exhaustive."}, Verify: verify,
			Files: []string{"backend/internal/domain/user.go"},
		},
		{
			ID: "tests", Title: "Tests", Description: "Add behavior tests.", Dependencies: []string{"model"},
			AcceptanceCriteria: []string{"Tests cover invalid users."}, Verify: verify,
			Files: []string{"backend/internal/domain/user_test.go"},
		},
	}}
}

// Accepting a plan must leave every task carrying its own scope and every task
// pair carrying a stored verdict, so scheduling and integration can read the
// decision back without recomputing it.
func TestAcceptedPlanPersistsTaskScopesAndPairVerdicts(t *testing.T) {
	c, _, runID := newMasterFixture(t, conflictingMasterPlan(), domain.WorkflowPlanApprovalManual)
	ctx := context.Background()
	detail, err := c.GeneratePlan(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Plan.Status != domain.WorkflowPlanValidated || len(detail.Tasks) != 3 {
		t.Fatalf("plan=%+v tasks=%d", detail.Plan, len(detail.Tasks))
	}
	byStep := map[string]domain.WorkflowTask{}
	for _, task := range detail.Tasks {
		byStep[task.PlanStepID] = task
		if task.ScopeJSON == "" || task.ScopeJSON == "{}" {
			t.Fatalf("task %s persisted no scope", task.PlanStepID)
		}
	}

	graph, err := c.LoadTaskGraph(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Relationships) != 3 {
		t.Fatalf("relationships=%d, want one per unordered pair", len(graph.Relationships))
	}

	model, roles, tests := byStep["model"].ID, byStep["roles"].ID, byStep["tests"].ID

	conflict, ok := graph.RelationFor(roles, model)
	if !ok {
		t.Fatalf("no verdict stored for (model, roles)")
	}
	if conflict.Relation != domain.WorkflowTaskRelationWriteConflict {
		t.Fatalf("model/roles relation=%q, want probable_write_conflict", conflict.Relation)
	}
	if conflict.Reason != string(workflowcore.TaskRelationReasonSharedFileWrite) {
		t.Fatalf("reason=%q, want a shared-file reason", conflict.Reason)
	}
	if len(conflict.Overlap) != 1 || conflict.Overlap[0] != "backend/internal/domain/user.go" {
		t.Fatalf("overlap=%+v, want the declared shared file", conflict.Overlap)
	}
	if conflict.Detail == "" {
		t.Fatal("conflict persisted without a human-readable reason")
	}

	dep, ok := graph.RelationFor(model, tests)
	if !ok || dep.Relation != domain.WorkflowTaskRelationDependency {
		t.Fatalf("model/tests verdict=%+v ok=%v, want functional_dependency", dep, ok)
	}

	// The dependency and the conflict must both be visible from the task's own
	// scope, which is what a scheduler reads.
	modelScope := graph.Scopes[model]
	if modelScope.ExecutionStrategy != domain.WorkflowTaskExecutionSerialized {
		t.Fatalf("model strategy=%q, want serialized", modelScope.ExecutionStrategy)
	}
	if !hasPath(modelScope.WritePaths, "backend/internal/domain/user.go") {
		t.Fatalf("model write paths=%+v", modelScope.WritePaths)
	}
	if !hasPath(modelScope.Components, "backend") {
		t.Fatalf("model components=%+v, want backend", modelScope.Components)
	}
	if got := graph.ConflictsFor(model); len(got) != 1 || got[0] != roles {
		t.Fatalf("conflicts for model=%+v, want [%s]", got, roles)
	}
	testsScope := graph.Scopes[tests]
	if testsScope.ExecutionStrategy != domain.WorkflowTaskExecutionSequential {
		t.Fatalf("tests strategy=%q, want sequential", testsScope.ExecutionStrategy)
	}
	if len(testsScope.IntegrationDependencies) != 1 || testsScope.IntegrationDependencies[0] != model {
		t.Fatalf("tests integration deps=%+v, want [%s]", testsScope.IntegrationDependencies, model)
	}
}

// Re-entering plan finalization must not duplicate or corrupt the stored
// graph: relationships are keyed by the canonical pair, so the second pass
// upserts the same three rows.
func TestPlanRegenerationDoesNotDuplicateStoredRelationships(t *testing.T) {
	c, _, runID := newMasterFixture(t, conflictingMasterPlan(), domain.WorkflowPlanApprovalManual)
	ctx := context.Background()
	if _, err := c.GeneratePlan(ctx, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GeneratePlan(ctx, runID); err != nil {
		t.Fatal(err)
	}
	graph, err := c.LoadTaskGraph(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Relationships) != 3 {
		t.Fatalf("relationships=%d after regeneration, want 3", len(graph.Relationships))
	}
}

// A plan that declares no scope at all still gets a well-formed graph: every
// task carries a scope document and every pair carries a verdict.
func TestPlanWithoutDeclaredScopeStillPersistsAGraph(t *testing.T) {
	c, _, runID := newMasterFixture(t, validMasterPlan(), domain.WorkflowPlanApprovalManual)
	ctx := context.Background()
	if _, err := c.GeneratePlan(ctx, runID); err != nil {
		t.Fatal(err)
	}
	graph, err := c.LoadTaskGraph(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Scopes) != 2 || len(graph.Relationships) != 1 {
		t.Fatalf("scopes=%d relationships=%d, want 2 and 1", len(graph.Scopes), len(graph.Relationships))
	}
	for id, scope := range graph.Scopes {
		if scope.Version != workflowcore.TaskGraphPolicyVersion {
			t.Fatalf("task %s scope version=%q", id, scope.Version)
		}
		if scope.ExecutionStrategy == "" {
			t.Fatalf("task %s persisted no execution strategy", id)
		}
	}
	if graph.Relationships[0].Relation != domain.WorkflowTaskRelationDependency {
		t.Fatalf("relation=%q, want functional_dependency", graph.Relationships[0].Relation)
	}
}
