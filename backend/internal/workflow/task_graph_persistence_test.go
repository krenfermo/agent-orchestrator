package workflow_test

import (
	"context"
	"encoding/json"
	"strings"
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

// safeOverlapMasterPlan is conflictingMasterPlan's two overlapping steps, with
// the overlap explicitly declared safe by one of them.
func safeOverlapMasterPlan() workflowcore.MasterPlan {
	plan := conflictingMasterPlan()
	plan.Steps[1].SafeWriteOverlaps = []workflowcore.PlannedSafeOverlap{{
		With:   "model",
		Paths:  []string{"backend/internal/domain/user.go"},
		Reason: "append-only type block, the two steps add distinct declarations",
	}}
	return plan
}

// The waiver, its reason, and the verdict it produced must all be durable: a
// process that reads the plan back later must see "independent, declared safe,
// here is why" without re-running the classifier.
func TestDeclaredSafeOverlapIsPersistedAndSurvivesReload(t *testing.T) {
	c, _, runID := newMasterFixture(t, safeOverlapMasterPlan(), domain.WorkflowPlanApprovalManual)
	ctx := context.Background()
	detail, err := c.GeneratePlan(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Plan.Status != domain.WorkflowPlanValidated {
		t.Fatalf("plan status=%q validation=%s", detail.Plan.Status, detail.Plan.ValidationJSON)
	}
	byStep := map[string]domain.WorkflowTask{}
	for _, task := range detail.Tasks {
		byStep[task.PlanStepID] = task
	}
	model, roles := byStep["model"].ID, byStep["roles"].ID

	// Read the graph back through the store, not from the in-memory result.
	graph, err := c.LoadTaskGraph(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	rel, ok := graph.RelationFor(model, roles)
	if !ok {
		t.Fatal("no verdict stored for the waived pair")
	}
	if rel.Relation != domain.WorkflowTaskRelationIndependent {
		t.Fatalf("relation=%q, want independent once the overlap is declared safe", rel.Relation)
	}
	if rel.Reason != string(workflowcore.TaskRelationReasonDeclaredSafeOverlap) {
		t.Fatalf("reason=%q, want %q", rel.Reason, workflowcore.TaskRelationReasonDeclaredSafeOverlap)
	}
	if !hasPath(rel.Overlap, "backend/internal/domain/user.go") {
		t.Fatalf("overlap=%+v, want the waived path stored", rel.Overlap)
	}
	if !strings.Contains(rel.Detail, "append-only type block") {
		t.Fatalf("detail=%q, want the waiver reason stored", rel.Detail)
	}
	if got := graph.ConflictsFor(model); len(got) != 0 {
		t.Fatalf("conflicts for model=%+v, want none", got)
	}
	if got := graph.Scopes[roles].ExecutionStrategy; got != domain.WorkflowTaskExecutionParallel {
		t.Fatalf("roles strategy=%q, want parallel", got)
	}

	// The waiver itself is on the task record, so a later re-classification
	// (which reads persisted scopes, not plan text) applies it again.
	waivers := graph.Scopes[roles].SafeWriteOverlaps
	if len(waivers) != 1 || waivers[0].WithTaskID != model || waivers[0].Reason == "" {
		t.Fatalf("stored waivers=%+v, want one resolved to %s with a reason", waivers, model)
	}
}

// A waiver naming a step that does not exist, or stating no reason, is a plan
// error -- not something quietly dropped, which would leave a real overlap
// looking reviewed when nobody reviewed it.
func TestPlanValidationRejectsMalformedSafeWriteOverlap(t *testing.T) {
	for _, tc := range []struct {
		name   string
		waiver workflowcore.PlannedSafeOverlap
		want   string
	}{
		{"unknown step", workflowcore.PlannedSafeOverlap{With: "nope", Reason: "because"}, "unknown step"},
		{"self", workflowcore.PlannedSafeOverlap{With: "roles", Reason: "because"}, "with itself"},
		{"no reason", workflowcore.PlannedSafeOverlap{With: "model"}, "without a reason"},
		{"no target", workflowcore.PlannedSafeOverlap{Reason: "because"}, "no target step"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := conflictingMasterPlan()
			plan.Steps[1].SafeWriteOverlaps = []workflowcore.PlannedSafeOverlap{tc.waiver}
			_, v, _ := workflowcore.NormalizeAndValidatePlan(plan, plan.Objective, workflowcore.MaxPlanSteps)
			if v.Valid || !strings.Contains(strings.Join(v.Errors, " "), tc.want) {
				t.Fatalf("validation=%+v, want an error containing %q", v, tc.want)
			}
		})
	}
}

// A plan that declares no waiver must serialize -- and therefore hash --
// exactly as it did before the field existed.
func TestUndeclaredSafeOverlapsDoNotChangeThePlanHash(t *testing.T) {
	plan := conflictingMasterPlan()
	normalized, v, hash := workflowcore.NormalizeAndValidatePlan(plan, plan.Objective, workflowcore.MaxPlanSteps)
	if !v.Valid {
		t.Fatal(v.Errors)
	}
	for _, step := range normalized.Steps {
		if step.SafeWriteOverlaps != nil {
			t.Fatalf("step %q normalized an undeclared waiver list to non-nil", step.ID)
		}
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "safeWriteOverlaps") {
		t.Fatalf("undeclared waivers leaked into the canonical plan JSON: %s", raw)
	}
	_, _, again := workflowcore.NormalizeAndValidatePlan(plan, plan.Objective, workflowcore.MaxPlanSteps)
	if hash != again {
		t.Fatalf("hash is not stable: %s vs %s", hash, again)
	}
}
