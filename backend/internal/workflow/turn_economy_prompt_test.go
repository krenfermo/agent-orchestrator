package workflow_test

import (
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// turn_economy_prompt_test.go -- Checkpoint P7's guidance goes to a TASK and
// only to a TASK, and adding it changed nothing for anybody else.

func taskArtifact(strategy domain.ExecutionStrategy) workflowcore.PlanArtifact {
	a := workflowcore.BuildPlanArtifact("proj-1", "show the entity name", "v1")
	a.Strategy = strategy
	return a
}

func TestTurnEconomyGuidanceGoesToATaskOnly(t *testing.T) {
	const marker = "How to spend your turns on this task"
	if got := workflowcore.BuildWorkStepPrompt(taskArtifact(domain.ExecutionStrategyTask)); !strings.Contains(got, marker) {
		t.Error("a TASK's work prompt must carry the turn-economy section")
	}
	for _, s := range []domain.ExecutionStrategy{
		domain.ExecutionStrategyAutonomous,
		domain.ExecutionStrategyMaster,
		"",
	} {
		if got := workflowcore.BuildWorkStepPrompt(taskArtifact(s)); strings.Contains(got, marker) {
			t.Errorf("strategy %q must NOT get the turn-economy section: an open-ended run "+
				"asked not to explore has been asked not to do its job", s)
		}
	}
}

// The section is APPENDED. Every prompt built before P7 -- and every artifact
// still in the database carrying no strategy -- must be byte-identical to what
// it was, or a restart would rebuild a work prompt that differs from the one
// the worker was actually given.
func TestWorkPromptIsUnchangedWithoutAStrategy(t *testing.T) {
	plain := workflowcore.BuildWorkStepPrompt(taskArtifact(""))
	task := workflowcore.BuildWorkStepPrompt(taskArtifact(domain.ExecutionStrategyTask))
	if !strings.HasPrefix(task, plain) {
		t.Fatal("the TASK prompt must be the unchanged prompt plus a suffix, not a rewrite of it")
	}
	if len(task) <= len(plain) {
		t.Fatal("the TASK prompt must actually carry the extra section")
	}
}

// What the section says has to survive review by a person, so the four habits
// it exists to encourage are pinned by name rather than by length.
func TestTurnEconomyGuidanceNamesTheHabitsItIsFor(t *testing.T) {
	got := workflowcore.BuildWorkStepPrompt(taskArtifact(domain.ExecutionStrategyTask))
	for _, want := range []string{
		"several related questions",               // batching
		"Do not survey the repository",            // scoped reading
		"OUTSIDE a turn",                          // waiting off the model
		"Verify once",                             // no redundant re-verification
		"Keep the messages between actions short", // output discipline
		"Delegate the QUESTION only",              // scoped subagents, not delegated implementation
	} {
		if !strings.Contains(got, want) {
			t.Errorf("turn-economy section lost %q", want)
		}
	}
	// And the one thing it must NOT do: loosen what AO tracks. The change, the
	// commits and the report stay in this session.
	if !strings.Contains(got, "stay in this session") {
		t.Error("delegation guidance must keep the change itself in the AO session")
	}
}
