package workflow_test

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// context_sources_test.go -- Frente 3 / 3C, instrument I3: every run records,
// in the policy_snapshot its creation writes, which context decorators the
// creating daemon had active. It is the only durable answer to "which arm of
// the memory A/B was this run in".

func TestRunCreationFreezesContextSources(t *testing.T) {
	ctx := context.Background()
	store, _ := newCrashFixture(t, validMasterPlan())
	arm := domain.ContextSourcesSnapshot{MemoryMode: "assisted", ContextRouter: "off"}
	c := workflowcore.New(workflowcore.Deps{Store: store, Projects: store, Planner: &staticPlanner{plan: validMasterPlan()}, PlannerContextBuilder: staticContext{}, ContextSources: arm})

	task, err := c.CreateTaskRun(ctx, workflowcore.TaskRunRequest{ProjectID: "p", Objective: "Fix the typo", Strategy: explicitStrategy(t, domain.ExecutionStrategyTask)})
	if err != nil {
		t.Fatal(err)
	}
	objective, err := c.CreateObjectiveRunWithStrategy(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual, explicitStrategy(t, domain.ExecutionStrategyAutonomous))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{task.Run.ID, objective.Run.ID} {
		if got := runPolicy(t, store, id).ContextSources; got != arm {
			t.Fatalf("%s: context sources = %+v, want %+v", id, got, arm)
		}
	}

	// The execution-policy freeze rewrites the snapshot after creation; the
	// arm must survive it.
	if err := c.ApplyExecutionPolicySnapshot(ctx, task.Run.ID, domain.UserID("owner-1"), nil); err != nil {
		t.Fatal(err)
	}
	if got := runPolicy(t, store, task.Run.ID).ContextSources; got != arm {
		t.Fatalf("after the execution-policy freeze: context sources = %+v, want %+v", got, arm)
	}

	// A daemon that stamps nothing records nothing: never a guessed "off".
	bare := workflowcore.New(workflowcore.Deps{Store: store, Projects: store})
	legacy, err := bare.CreateTaskRun(ctx, workflowcore.TaskRunRequest{ProjectID: "p", Objective: "Another", Strategy: explicitStrategy(t, domain.ExecutionStrategyTask)})
	if err != nil {
		t.Fatal(err)
	}
	if got := runPolicy(t, store, legacy.Run.ID).ContextSources; got.Recorded() {
		t.Fatalf("unstamped coordinator recorded %+v", got)
	}
}
