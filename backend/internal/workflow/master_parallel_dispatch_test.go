package workflow_test

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

func parallelDispatchPlan(deps map[string][]string) workflowcore.MasterPlan {
	steps := []workflowcore.PlannedStep{}
	for i, id := range []string{"a", "b", "c"} {
		steps = append(steps, workflowcore.PlannedStep{
			ID: id, Title: "Task " + id,
			Description:  "Update backend/internal/workflow/parallel_" + id + ".go.",
			Dependencies: deps[id], AcceptanceCriteria: []string{"The task " + id + " behavior works."},
			Verify: workflowcore.VerificationPlan{Commands: []workflowcore.VerificationCommandCheck{{Command: "go", Args: []string{"test", "./internal/workflow"}, RequiredExitCode: 0, RetrySafe: true}}},
		})
		_ = i
	}
	return workflowcore.MasterPlan{Version: "v1", Objective: "Build users", Summary: "parallel dispatch", Steps: steps}
}

func approveParallelPlan(t *testing.T, deps map[string][]string) workflowcore.RunDetail {
	t.Helper()
	c, runID := newMasterFixtureWithExecutionMode(t, parallelDispatchPlan(deps), domain.ExecutionSmartParallelWorktrees)
	ctx := context.Background()
	if _, err := c.GeneratePlan(ctx, runID); err != nil {
		t.Fatal(err)
	}
	detail, err := c.ApprovePlan(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	return detail
}

func waitingReason(t *testing.T, task domain.WorkflowTask) domain.WorkflowTaskWaitingReason {
	t.Helper()
	scope, err := workflowcore.UnmarshalTaskScope(task.ScopeJSON)
	if err != nil {
		t.Fatal(err)
	}
	return scope.WaitingReason
}

func TestMasterDispatchesThreeIndependentTasksConcurrently(t *testing.T) {
	detail := approveParallelPlan(t, map[string][]string{})
	for _, task := range detail.Tasks {
		if task.State != domain.WorkflowTaskRunning || task.ExecutionRunID == nil {
			t.Fatalf("task %s = state %q run %v, want concurrently running", task.PlanStepID, task.State, task.ExecutionRunID)
		}
		if got := waitingReason(t, task); got != "" {
			t.Fatalf("task %s wait=%q", task.PlanStepID, got)
		}
	}
}

func TestIsolatedWorktreeKeepsIndependentTasksSerial(t *testing.T) {
	c, runID := newMasterFixtureWithExecutionMode(t, parallelDispatchPlan(map[string][]string{}), domain.ExecutionIsolatedWorktree)
	ctx := context.Background()
	if _, err := c.GeneratePlan(ctx, runID); err != nil {
		t.Fatal(err)
	}
	detail, err := c.ApprovePlan(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	running := 0
	for _, task := range detail.Tasks {
		if task.ExecutionRunID != nil {
			running++
		}
	}
	if running != 1 {
		t.Fatalf("isolated_worktree dispatched %d independent tasks, want exactly 1 without smart-parallel opt-in", running)
	}
}

func TestMasterDependencyChainPersistsWaitingReason(t *testing.T) {
	detail := approveParallelPlan(t, map[string][]string{"b": {"a"}, "c": {"b"}})
	if detail.Tasks[0].State != domain.WorkflowTaskRunning {
		t.Fatalf("A state=%q", detail.Tasks[0].State)
	}
	for _, task := range detail.Tasks[1:] {
		if task.ExecutionRunID != nil || waitingReason(t, task) != domain.WorkflowTaskWaitingDependency {
			t.Fatalf("task %s dispatched=%v wait=%q", task.PlanStepID, task.ExecutionRunID != nil, waitingReason(t, task))
		}
	}
}

func TestMasterDispatchesABWhileCWaitsForBoth(t *testing.T) {
	detail := approveParallelPlan(t, map[string][]string{"c": {"a", "b"}})
	for _, task := range detail.Tasks[:2] {
		if task.State != domain.WorkflowTaskRunning || task.ExecutionRunID == nil {
			t.Fatalf("task %s not running", task.PlanStepID)
		}
	}
	c := detail.Tasks[2]
	if c.ExecutionRunID != nil || waitingReason(t, c) != domain.WorkflowTaskWaitingDependency {
		t.Fatalf("C dispatched=%v wait=%q", c.ExecutionRunID != nil, waitingReason(t, c))
	}
}

func TestMasterPersistsWriteConflictWaitingReason(t *testing.T) {
	plan := parallelDispatchPlan(map[string][]string{})
	plan.Steps[0].Description = "Update backend/internal/workflow/shared.go."
	plan.Steps[1].Description = "Also update backend/internal/workflow/shared.go."
	c, runID := newMasterFixtureWithExecutionMode(t, plan, domain.ExecutionSmartParallelWorktrees)
	ctx := context.Background()
	if _, err := c.GeneratePlan(ctx, runID); err != nil {
		t.Fatal(err)
	}
	detail, err := c.ApprovePlan(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]domain.WorkflowTask{}
	for _, task := range detail.Tasks {
		byID[task.PlanStepID] = task
	}
	if byID["a"].ExecutionRunID == nil {
		t.Fatal("A was not dispatched")
	}
	if byID["b"].ExecutionRunID != nil || waitingReason(t, byID["b"]) != domain.WorkflowTaskWaitingConflict {
		t.Fatalf("B dispatched=%v wait=%q", byID["b"].ExecutionRunID != nil, waitingReason(t, byID["b"]))
	}
}
